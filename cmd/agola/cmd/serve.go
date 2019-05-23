// Copyright 2019 Sorint.lab
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"context"
	"fmt"

	"github.com/sorintlab/agola/cmd"
	"github.com/sorintlab/agola/internal/services/config"
	"github.com/sorintlab/agola/internal/services/configstore"
	"github.com/sorintlab/agola/internal/services/executor"
	rsexecutor "github.com/sorintlab/agola/internal/services/executor"
	"github.com/sorintlab/agola/internal/services/gateway"
	"github.com/sorintlab/agola/internal/services/gitserver"
	"github.com/sorintlab/agola/internal/services/notification"
	rsscheduler "github.com/sorintlab/agola/internal/services/runservice"
	"github.com/sorintlab/agola/internal/services/scheduler"
	"github.com/sorintlab/agola/internal/util"

	"github.com/spf13/cobra"
	"go.etcd.io/etcd/embed"
	errors "golang.org/x/xerrors"
)

var (
	// default gatewayURL
	gatewayURL = fmt.Sprintf("http://%s:%d", "localhost", 8000)
)

var componentsNames = []string{
	"all",
	"gateway",
	"scheduler",
	"notification",
	"runservice",
	"executor",
	"configstore",
	"gitserver",
}

var cmdServe = &cobra.Command{
	Use:     "serve",
	Short:   "serve",
	Version: cmd.Version,
	Run: func(cmd *cobra.Command, args []string) {
		if err := serve(cmd, args); err != nil {
			log.Fatalf("err: %v", err)
		}
	},
}

type serveOptions struct {
	config              string
	components          []string
	embeddedEtcd        bool
	embeddedEtcdDataDir string
}

var serveOpts serveOptions

func init() {
	flags := cmdServe.PersistentFlags()

	flags.StringVar(&serveOpts.config, "config", "./config.yml", "config file path")
	flags.StringSliceVar(&serveOpts.components, "components", []string{}, `list of components to start (specify "all" to start all components)`)
	flags.BoolVar(&serveOpts.embeddedEtcd, "embedded-etcd", false, "start and use an embedded etcd, only for testing purpose")
	flags.StringVar(&serveOpts.embeddedEtcdDataDir, "embedded-etcd-data-dir", "/tmp/agola/etcd", "embedded etcd data dir, only for testing purpose")

	cmdServe.MarkFlagRequired("config")

	cmdAgola.AddCommand(cmdServe)
}

func embeddedEtcd(ctx context.Context) error {
	cfg := embed.NewConfig()
	cfg.Dir = serveOpts.embeddedEtcdDataDir
	cfg.Logger = "zap"
	cfg.LogOutputs = []string{"stderr"}

	log.Infof("starting embedded etcd server")
	e, err := embed.StartEtcd(cfg)
	if err != nil {
		return err
	}

	go func() {
		select {
		case <-e.Server.ReadyNotify():
			log.Infof("embedded etcd server is ready")
		}
		select {
		case <-ctx.Done():
			log.Infof("stopping embedded etcd server")
			e.Close()
		}
	}()

	return nil
}

func isComponentEnabled(name string) bool {
	if util.StringInSlice(serveOpts.components, "all") {
		return true
	}
	return util.StringInSlice(serveOpts.components, name)
}

func serve(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	if len(serveOpts.components) == 0 {
		return errors.Errorf("no enabled components")
	}
	for _, ec := range serveOpts.components {
		if !util.StringInSlice(componentsNames, ec) {
			return errors.Errorf("unkown component name %q", ec)
		}
	}

	c, err := config.Parse(serveOpts.config)
	if err != nil {
		return errors.Errorf("config error: %w", err)
	}

	if serveOpts.embeddedEtcd {
		if err := embeddedEtcd(ctx); err != nil {
			return errors.Errorf("failed to start run service scheduler: %w", err)
		}
	}

	var rs *rsscheduler.Runservice
	if isComponentEnabled("runservice") {
		rs, err = rsscheduler.NewRunservice(ctx, &c.Runservice)
		if err != nil {
			return errors.Errorf("failed to start run service scheduler: %w", err)
		}
	}

	var ex *rsexecutor.Executor
	if isComponentEnabled("executor") {
		ex, err = executor.NewExecutor(&c.Executor)
		if err != nil {
			return errors.Errorf("failed to start run service executor: %w", err)
		}
	}

	var cs *configstore.Configstore
	if isComponentEnabled("configstore") {
		cs, err = configstore.NewConfigstore(ctx, &c.Configstore)
		if err != nil {
			return errors.Errorf("failed to start config store: %w", err)
		}
	}

	var sched *scheduler.Scheduler
	if isComponentEnabled("scheduler") {
		sched, err = scheduler.NewScheduler(&c.Scheduler)
		if err != nil {
			return errors.Errorf("failed to start scheduler: %w", err)
		}
	}

	var ns *notification.NotificationService
	if isComponentEnabled("notification") {
		ns, err = notification.NewNotificationService(c)
		if err != nil {
			return errors.Errorf("failed to start notification service: %w", err)
		}
	}

	var gw *gateway.Gateway
	if isComponentEnabled("gateway") {
		gw, err = gateway.NewGateway(c)
		if err != nil {
			return errors.Errorf("failed to start gateway: %w", err)
		}
	}

	var gs *gitserver.Gitserver
	if isComponentEnabled("gitserver") {
		gs, err = gitserver.NewGitserver(&c.Gitserver)
		if err != nil {
			return errors.Errorf("failed to start git server: %w", err)
		}
	}

	errCh := make(chan error)

	if rs != nil {
		go func() { errCh <- rs.Run(ctx) }()
	}
	if ex != nil {
		go func() { errCh <- ex.Run(ctx) }()
	}
	if cs != nil {
		go func() { errCh <- cs.Run(ctx) }()
	}
	if sched != nil {
		go func() { errCh <- sched.Run(ctx) }()
	}
	if ns != nil {
		go func() { errCh <- ns.Run(ctx) }()
	}
	if gw != nil {
		go func() { errCh <- gw.Run(ctx) }()
	}
	if gs != nil {
		go func() { errCh <- gs.Run(ctx) }()
	}

	return <-errCh
}
