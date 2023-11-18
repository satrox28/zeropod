package main

import (
	"context"

	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/manager"
	"github.com/containerd/containerd/v2/pkg/shim"
	_ "github.com/ctrox/zeropod/runc/task/plugin"
	"github.com/ctrox/zeropod/zeropod"
)

func main() {
	shim.Run(context.Background(), manager.NewShimManager(zeropod.RuntimeName))
}
