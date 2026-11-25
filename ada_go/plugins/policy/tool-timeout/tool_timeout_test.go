package tooltimeout_test

import (
	"context"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	tooltimeout "github.com/issueye/ada_go_v2/plugins/policy/tool-timeout"
	seamtools "github.com/issueye/ada_go_v2/plugins/seam/tools"
)

func TestTightensToolTimeout(t *testing.T) {
	app := kernel.New(kernel.Options{})
	defer func() { _ = app.Stop(context.Background()) }()
	if _, err := app.Plugin(seamtools.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Plugin(tooltimeout.Plugin(), kernel.NewConfig(map[string]any{"defaultMs": 20})); err != nil {
		t.Fatal(err)
	}
	reg, err := kernel.Service[*seamtools.Registry](app.Root(), "tools")
	if err != nil {
		t.Fatal(err)
	}
	_, err = reg.Register(seamtools.ToolDef{
		Name: "slow",
		Handler: func(context.Context, seamtools.CallInfo) (seamtools.Result, error) {
			time.Sleep(80 * time.Millisecond)
			return seamtools.Result{Status: seamtools.StatusOK}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := reg.Invoke(kernel.WithPrincipal(context.Background(), "p"), seamtools.CallInfo{Name: "slow"})
	if err != nil || res.Status != seamtools.StatusTimeout {
		t.Fatalf("应被策略收紧为超时: %+v %v", res, err)
	}
}
