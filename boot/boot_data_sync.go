package boot

import (
	"context"
	"fmt"

	"common/ha"
	"datariver/app/service"
	"datariver/lib/global"

	"github.com/pkg/errors"
)

func get_key() string {
	return fmt.Sprintf("/lock/%s/%s", global.SERVERNAME, global.GConfig.BrokerConfig.Group)
}

func StartDataSyncServer() error {
	ha, err := ha.NewHaWrapper(global.GConfig.BrokerConfig.EtcdAddr, get_key(), 3,
		"no-use", &service.DataSyncServer{})
	if err != nil {
		return errors.Wrap(err, "启动同步服务失败")
	}
	go ha.Run(context.Background())

	return nil
}
