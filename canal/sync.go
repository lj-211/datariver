package canal

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/siddontang/go-mysql/canal"
	"github.com/siddontang/go-mysql/mysql"

	"common/ha"
	"datariver/config"
	//"yunwa.com/datariver/queue"
)

const (
	default_key       = "TABLENAME"
	default_key_index = -1
)

var server_id uint32

type SyncClient struct {
	sync.RWMutex
	cfg           *config.ServerConfig
	canal         *canal.Canal
	rules         map[string]*TopicInfo
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	wgCfg         sync.WaitGroup
	master        *MetaInfo
	MysqlDumpPath string
	syncCh        chan interface{}
	roleCh        chan ha.RoleType
	errCh         chan error
	isRunaway     bool
	runAwayMsg    string
	sourceLock    sync.RWMutex
	isClose       bool
	LastMeta      *MetaInfo
}

func NewSyncClient(cfg *config.ServerConfig) (*SyncClient, error) {
	c := new(SyncClient)
	c.cfg = cfg
	c.rules = make(map[string]*TopicInfo)
	c.syncCh = make(chan interface{}, 4096)
	c.roleCh = make(chan ha.RoleType)

	c.ctx, c.cancel = context.WithCancel(context.Background())

	var err error
	/*
		// todo: 初始化queue
		if err = queue.Init(cfg.BrokerConfig.KafkaAddr); err != nil {
			return nil, err
		}
	*/
	if err = c.newCanal(); err != nil {
		return nil, err
	}
	if err = c.loadPositionInfo(); err != nil {
		return nil, err
	}

	if err = c.parseSourceRule(); err != nil {
		return nil, err
	}

	// binlog must be in "row" mode
	if err = c.canal.CheckBinlogRowImage("FULL"); err != nil {
		return nil, err
	}

	return c, nil
}

func (r *SyncClient) Close() {
	r.isClose = true
	//log.Info("closing SyncClient")

	r.cancel()
	//log.Info("closing SyncClient cancel ")

	r.canal.Close()
	//log.Info("closing SyncClient canal.Close ")

	r.master.Close()
	//log.Info("closing SyncClient master.Close ")
	//log.Info("closing SyncClient wg.Wait Begin")
	r.wg.Wait()
	//log.Info("closing SyncClient wg.Wait End ")
	//log.Info("begin CloseProducer")

	// todo: queue
	//err := queue.CloseProducer()

	//log.Info("end CloseProducer err:", err)
}

func (c *SyncClient) RoleSwitch() chan<- ha.RoleType {
	return c.roleCh
}

func (c *SyncClient) loadPositionInfo() error {
	sql := fmt.Sprintf(`SHOW MASTER STATUS;`)
	var err error
	for loop := true; loop; loop = false {
		c.master, err = NewMetaInfo(c.cfg.BrokerConfig.Group)
		if err != nil {
			//log.Info("Get NewMetaInfo with err:%+v, refresh from mysql server:%+v", err, c.cfg.SourceConfig.MysqlConn)
			res := &mysql.Result{}
			if res, err = c.canal.Execute(sql); err != nil {
				break
			}
			pos := mysql.Position{}
			for i := 0; i < res.Resultset.RowNumber(); i++ {
				if pos.Name, err = res.GetString(i, 0); err != nil {
					break
				}
				var t int64
				if t, err = res.GetInt(i, 1); err != nil {
					break
				}
				pos.Pos = uint32(t)
				break
			}
			c.master = &MetaInfo{Group: c.cfg.BrokerConfig.Group, MyRole: ha.Master}
			c.master.Save(pos)
		}
	}

	//log.Info("[-]SyncClient loadPositionInfo, name:%+v, pos:%+v, err:%+v ", c.master.Name, c.master.Pos, err)

	return err
}

//按照官方文档需要生产不同的server_id, 但是阿里的mysql即使生成相同的server_id也不会有问题
//这里以官方为准
func genMysqlSlaveServerId(group string) uint32 {
	b := make([]byte, 32)
	rand.Read(b)
	s := base64.StdEncoding.EncodeToString(b)
	fnv_hash := fnv.New32()
	fnv_hash.Write([]byte(s))
	return fnv_hash.Sum32()
}

func (c *SyncClient) newCanal() error {
	cfg := canal.NewDefaultConfig()
	cfg.Addr = c.cfg.SourceConfig.DBConfig.Addr
	cfg.User = c.cfg.SourceConfig.DBConfig.User
	cfg.Password = c.cfg.SourceConfig.DBConfig.Passwd
	cfg.Flavor = "mysql"

	if server_id > 0 {
		cfg.ServerID = server_id
	} else {
		cfg.ServerID = genMysqlSlaveServerId(c.cfg.BrokerConfig.Group)
		server_id = cfg.ServerID
	}

	cfg.Dump.DiscardErr = false

	var err error
	if c.canal, err = canal.NewCanal(cfg); err != nil {
		//log.Info("NewCanal err:%+v", err)
		return err
	}
	c.canal.SetEventHandler(&eventHandler{c})
	return err
}

func (c *SyncClient) Start() error {
	c.wg.Add(1)
	go c.syncLoop()

	pos := c.master.Position()
	// todo: StartFromGTID
	if err := c.canal.RunFrom(pos); err != nil {
		c.errCh <- err
		return err
	}
	// todo: 暂时不支持动态更新配置
	// go c.watcherConfig(c.ctx)
	return nil
}

func (c *SyncClient) Ctx() context.Context {
	return c.ctx
}

func (c *SyncClient) IsRunaway() (bool, string) {
	need_reboot_seconnds := time.Duration(c.cfg.SourceConfig.RebootSeconds) * time.Second
	if need_reboot_seconnds == 0 {
		need_reboot_seconnds = 30 * time.Minute
	}
	cur_meta, _ := NewMetaInfo(c.cfg.BrokerConfig.Group)
	if c.LastMeta == nil {
		c.LastMeta = cur_meta
	} else {
		//超过30分钟未更新可能就需要重启
		if cur_meta.Name != "" && c.LastMeta.Name == cur_meta.Name &&
			c.LastMeta.Pos == cur_meta.Pos {
			if need_reboot_seconnds > 0 && time.Now().Sub(cur_meta.LastSaveTime) > need_reboot_seconnds {
				return true, fmt.Sprintf("position not update for a long time(%+v), file:%+v, pos:%+v, last_time:%+v",
					need_reboot_seconnds, cur_meta.Name, cur_meta.Pos, cur_meta.LastSaveTime)
			}
		} else {
			c.LastMeta = cur_meta
		}
	}
	c.Lock()
	defer c.Unlock()
	return c.isRunaway, c.runAwayMsg
}

func (c *SyncClient) runaway(msg string) {
	c.Lock()
	defer c.Unlock()

	c.isRunaway = true
	c.runAwayMsg = msg
}

func (c *SyncClient) syncLoop() {
	defer c.wg.Done()
	var pos mysql.Position
	tick := time.NewTicker(3 * time.Second)
	var newPos, needSavePos bool
	for {
		if c.isClose {
			time.Sleep(2 * time.Second)
			//log.Info("SyncClient is Closing, waiting exit ")
			continue
		}
		needSavePos = false
		select {
		case <-tick.C:
			if newPos {
				needSavePos = true
			}
		case v := <-c.syncCh:
			switch v := v.(type) {
			case posSaver:
				newPos = true
				pos = v.pos
				if v.force {
					needSavePos = true
				}
			case EventData:
				// todo: queue
				/*
					if err := queue.PushBack(&v); err != nil {
						c.runaway(fmt.Sprintf("queue PushBack err:%+v", err))
						return
					}
				*/
			default:
				//log.Info("get syncCh %+v", v)
			}
		case <-c.ctx.Done():
			return

		case e := <-c.errCh:
			//log.Info("canal err pos:%+v, err:%+v", pos, e)
			c.runaway(e.Error())
			return
		}
		if needSavePos {
			if err := c.master.Save(pos); err != nil {
				//log.Error("save position to etcd err, pos:%+v, err:%+v, start retrySavePos", pos, err)
				if err := c.retrySavePos(pos); err != nil {
					//log.Error("SyncClient retrySavePos err:%+v", err)
					return
				}

			}
			newPos = false
		}
	}
}

func (c *SyncClient) retrySavePos(pos mysql.Position) error {
	var max_retry = int64((6 * time.Minute) / (5 * time.Second))
	tick := time.NewTicker(5 * time.Second)
	var cnt int64
	var err error
	for {
		if cnt >= max_retry {
			c.runaway(fmt.Sprintf("retrySavePos err:%+v", err))
			return fmt.Errorf("SyncClient runaway")
		}
		select {
		case <-tick.C:
			if err = c.master.Save(pos); err == nil {
				return nil
			} else {
				//log.Error("save position to etcd err, pos:%+v, max_retry:%+v, cnt:%+v, err:%+v, try save after 5 seconds", pos, max_retry, cnt, err)
			}
			cnt++
		case <-c.ctx.Done():
			return fmt.Errorf("SyncClient Done")
		}
	}
}

func ruleKey(schema string, table string) string {
	return fmt.Sprintf("%s:%s", strings.ToLower(schema), strings.ToLower(table))
}

//only source config
/*
func (c *SyncClient) watcherConfig(ctx context.Context) error {
	log.Info("start ")

	var environment = os.Getenv("GOENV")
	if environment == "" {
		environment = "online"
	} else {
		environment = strings.ToLower(environment)
	}
	log.Info("environment", environment)

	key := fmt.Sprintf("%v:%v", config.SERVERNAME, c.cfg.BrokerConfig.Group)
	w := ha.EtcdClient.Api().Watcher(ha.EtcdClient.ConfigKey(key, environment), nil)

	for {
		select {
		case <-ctx.Done():
			log.Error("watcherConfig Done key:%v", key)
			return fmt.Errorf("watcherConfig Done")
		default:
			if resp, err := w.Next(c.ctx); err == nil {
				log.Info("Next %+v", resp)
				if resp != nil && resp.Node != nil {
					if err = c.onConfigChanged(resp.Node.Key, resp.Node.Value); err != nil {
						log.Error("k:%v, node:%+v", *resp.Node)
					}
				}
			} else {
				log.Error("etcd next err:%+v", err)
				time.Sleep(time.Second * 10)
			}
		}
	}
	return nil
}

func (c *SyncClient) onConfigChanged(k, v string) error {
	log.Info("reload config k:%s, v:%s", k, v)
	var err error
	for loop := true; loop; loop = false {
		if err = json.Unmarshal([]byte(v), &c.cfg.SourceConfig); err != nil {
			break
		}
		if err = c.parseSourceRule(); err != nil {
			break
		}
	}
	log.Info("reload config k:%s, v:%s, cfg:%+v, err:%+v", k, v, c.cfg, err)
	return nil
}
*/

func (c *SyncClient) parseSourceRule() error {
	wildTables := make(map[string]bool)
	tmp_rule := make(map[string]*TopicInfo)
	var err error
OutLoop:
	for loop := true; loop; loop = false {
		if len(c.cfg.SourceConfig.Sources) == 0 && !c.cfg.SourceConfig.SyncAll {
			err = fmt.Errorf("THe Source config is empty, you may give a source configuration or set SyncAll=true")
			break OutLoop
		}
		for _, s := range c.cfg.SourceConfig.Sources {
			if len(s.Schema) == 0 {
				err = fmt.Errorf("empty schema not allowed for source")
				break OutLoop
			}
			if len(s.Tables) == 0 {
				tmp_rule[s.Schema] = &TopicInfo{Topic: strings.ToLower(s.Schema), Key: default_key, KeyIndex: default_key_index}
			}
			for _, table := range s.Tables {
				if len(table.Table) == 0 {
					err = fmt.Errorf("empty table not allowed for source")
					break OutLoop
				}
				//明确指定的配置才有效
				if regexp.QuoteMeta(table.Table) != table.Table {
					if _, ok := wildTables[ruleKey(s.Schema, table.Table)]; ok {
						err = fmt.Errorf("duplicate wildcard table defined for %s.%s", s.Schema, table.Table)
						break OutLoop
					}
					sql := fmt.Sprintf(`SELECT table_name FROM information_schema.tables WHERE
	    table_name RLIKE "%s" AND table_schema = "%s";`, table.Table, s.Schema)

					res, err2 := c.canal.Execute(sql)
					if err2 != nil {
						err = errors.Wrap(err2, "查询表名错误")
						break OutLoop
					}

					for i := 0; i < res.Resultset.RowNumber(); i++ {
						f, _ := res.GetString(i, 0)
						if r, err2 := c.genRule(&table, s.Schema, f); err2 == nil {
							tmp_rule[ruleKey(s.Schema, f)] = r
						} else {
							err = err2
							break OutLoop
						}
					}

					wildTables[ruleKey(s.Schema, table.Table)] = true
				} else {
					if r, err2 := c.genRule(&table, s.Schema, table.Table); err2 == nil {
						tmp_rule[ruleKey(s.Schema, table.Table)] = r
					} else {
						err = err2
						break OutLoop
					}
				}
			}
		}
	}
	if err == nil {
		c.sourceLock.Lock()
		defer c.sourceLock.Unlock()
		c.rules = tmp_rule
		//for k, v := range c.rules {
		//log.Info("rule source:%+v,  topic:%+v", k, *v)
		//}
	} else {
		//log.Error("config err:%+v, source:%+v", err, c.cfg.SourceConfig.Sources)
	}

	//log.Info("config err:%+v, source:%+v", err, c.cfg.SourceConfig.Sources)
	return err
}

func (c *SyncClient) genRule(source *config.TopicInfo, schema, table string) (*TopicInfo, error) {
	var err error
	rule := &TopicInfo{KeyIndex: default_key_index}
	rule.Topic, rule.Key = strings.ToLower(source.Topic), strings.ToLower(source.Key)
	for loop := true; loop; loop = false {
		if source.Topic != "" && source.Key != "" {
			rule.Topic, rule.Key = source.Topic, strings.ToLower(source.Key)
			tmp, err2 := c.canal.GetTable(schema, table)
			if err2 != nil {
				err = err2
				break
			}
			for i, item := range tmp.Columns {
				if rule.Key == strings.ToLower(item.Name) {
					rule.KeyIndex = i
					break
				}
			}
			if rule.KeyIndex == default_key_index {
				err = fmt.Errorf("%+v.%+v, source.ke not exist", schema, table, source.Key)
			}
			break
		}
		if source.Topic == "" && source.Key == "" {
			rule.Topic, rule.Key = strings.ToLower(schema), strings.ToLower(table)
			break
		}
		err = fmt.Errorf("topic，key value,  should be null or not null at the same time, schema:%+v, table:%+v", schema, table)
	}
	return rule, err
}

func (c *SyncClient) getFilterInfo(schema, table string) (*TopicInfo, bool) {
	c.sourceLock.RLock()
	defer c.sourceLock.RUnlock()
	var ret *TopicInfo
	var is_ok bool
	for loop := true; loop; loop = false {
		if rule, ok := c.rules[ruleKey(schema, table)]; ok {
			ret, is_ok = rule, true
			break
		}
		if rule, ok := c.rules[schema]; ok {
			if rule.Key == default_key {
				rule.Key = strings.ToLower(table)
			}
			ret, is_ok = rule, true
			break
		}
		if len(c.rules) == 0 {
			ret, is_ok = &TopicInfo{Topic: strings.ToLower(schema), Key: strings.ToLower(table), KeyIndex: default_key_index}, true
			break
		}
	}
	return ret, is_ok
}
