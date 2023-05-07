package task

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ctrox/zeropod/activator"
	"github.com/ctrox/zeropod/process"
	"github.com/ctrox/zeropod/runc"

	eventstypes "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/stdio"
	taskAPI "github.com/containerd/containerd/runtime/v2/task"
)

// StartZeropod starts a zeropod process
func (s *service) StartZeropod(ctx context.Context, r *taskAPI.StartRequest) error {
	container, err := s.getContainer(r.ID)
	if err != nil {
		return err
	}

	// switch to network ns of container and start our activator listener
	netNSPath, err := runc.GetNetworkNS(ctx, container.Bundle)
	if err != nil {
		return err
	}

	// create a new context in order to not run into deadline of parent context
	ctx = log.WithLogger(context.Background(), log.G(ctx).WithField("runtime", runc.RuntimeName))
	log.G(ctx).Printf("starting activator")

	// TODO: extract this port from container
	port := 5678
	srv, err := activator.NewServer(ctx, port, netNSPath)
	if err != nil {
		return err
	}

	s.shutdown.RegisterCallback(func(ctx context.Context) error {
		// stop server on shutdown
		srv.Stop(ctx)
		return nil
	})

	if err := srv.Start(ctx, func() (*runc.Container, process.Process, error) {
		log.G(ctx).Printf("got a request")

		// hold the send lock so that the start events are sent before any exit events in the error case
		s.eventSendMu.Lock()

		p, err := s.restore(ctx, container)
		if err != nil {
			// restore failed, this is currently unrecoverable, so we shutdown
			// our shim and let containerd recreate it.
			log.G(ctx).Fatalf("error restoring container, exiting shim: %s", err)
			os.Exit(1)
		}
		p.SetScaledDown(false)
		log.G(ctx).Printf("restored process: %d", p.Pid())

		s.send(&eventstypes.TaskStart{
			ContainerID: container.ID,
			Pid:         uint32(p.Pid()),
		})

		s.eventSendMu.Unlock()

		// before returning we set the net ns again as it might have changed
		// in the meantime. (not sure why that happens though)
		return container, p, nil
	}, func(container *runc.Container, p process.Process) error {
		time.Sleep(time.Second * 5)
		log.G(ctx).Info("scaling down after scaleup finished")
		return s.scaleDown(ctx, r, container, p)
	}); err != nil {
		log.G(ctx).Errorf("failed to start server on port %d: %s", port, err)
		return err
	}

	log.G(ctx).Printf("activator started")
	return nil
}

func (s *service) restore(ctx context.Context, container *runc.Container) (process.Process, error) {
	container.ID = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(time.Now().Unix()))))

	runtime := process.NewRunc("", container.Bundle, "k8s", "", "", false)

	// TODO: we should somehow reuse the original stdio. For now we just
	// create a file for stdout and stderr.
	s.stdio.Stdout = strings.TrimPrefix(s.stdio.Stdout, "file://")
	s.stdio.Stdout = strings.TrimSuffix(s.stdio.Stdout, "-1")
	s.stdio.Stderr = strings.TrimPrefix(s.stdio.Stdout, "file://")
	s.stdio.Stderr = strings.TrimSuffix(s.stdio.Stdout, "-1")

	p := process.New(container.ID, runtime, stdio.Stdio{
		Stdout: "file://" + s.stdio.Stdout + "-1",
		Stderr: "file://" + s.stdio.Stderr + "-1",
	})
	p.Bundle = container.Bundle
	p.Platform = s.platform
	p.WorkDir = filepath.Join(container.Bundle, "work")

	if p.CriuWorkPath == "" {
		// if criu work path not set, use container WorkDir
		p.CriuWorkPath = p.WorkDir
	}

	log.G(ctx).Infof("restoring %s", container.ID)

	if err := p.Create(ctx, &process.CreateConfig{
		ID:         container.ID,
		Bundle:     container.Bundle,
		Checkpoint: containerDir(container.Bundle),
	}); err != nil {
		return nil, fmt.Errorf("creation failed during restore: %w", err)
	}

	log.G(ctx).Info("restore: process created")

	if err := p.Start(ctx); err != nil {
		return nil, fmt.Errorf("start failed during restore: %w", err)
	}

	s.send(&eventstypes.TaskResumed{
		ContainerID: container.ID,
	})

	return p, nil
}

func snapshotDir(bundle string) string {
	return path.Join(bundle, "snapshots")
}

func containerDir(bundle string) string {
	return path.Join(snapshotDir(bundle), "container")
}
