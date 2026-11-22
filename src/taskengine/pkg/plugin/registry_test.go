package plugin

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

type stubPlugin struct {
	name  string
	optIn bool
}

func (s stubPlugin) Name() string                                                 { return s.name }
func (s stubPlugin) Init(context.Context, json.RawMessage) error                  { return nil }
func (s stubPlugin) Execute(context.Context, TaskPayload, ProgressReporter) error { return nil }
func (s stubPlugin) OptInOnly() bool                                              { return s.optIn }

func TestDefaultWorkerPluginsExcludesOptIn(t *testing.T) {
	r := NewRegistry()
	r.RegisterWorkerPlugin(stubPlugin{name: "video-transcoder"})
	r.RegisterWorkerPlugin(stubPlugin{name: "comfyui", optIn: true})
	r.RegisterWorkerPlugin(stubPlugin{name: "command-runner"})

	if got, want := r.DefaultWorkerPlugins(), []string{"command-runner", "video-transcoder"}; !reflect.DeepEqual(got, want) {
		t.Errorf("defaults = %v, want %v", got, want)
	}
	if _, ok := r.GetWorkerPlugin("comfyui"); !ok {
		t.Error("opt-in plugin must still be registered for explicit use")
	}
}
