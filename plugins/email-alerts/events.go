package main

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

type eventServer struct {
	pluginv1.UnimplementedEventConsumerServer
	app *App
}

// HandleEvent schedules a run after the settle delay. It answers at once:
// Silo allows 10 seconds per delivery, and the run itself happens a few
// seconds later.
func (s *eventServer) HandleEvent(ctx context.Context, req *pluginv1.HandleEventRequest) (*pluginv1.HandleEventResponse, error) {
	a := s.app
	a.ensureLoop()
	if req.GetEventName() == "library.item_added" || req.GetEventName() == "catalog.library.changed" {
		if p := req.GetPayload(); p != nil {
			if n, ok := p.AsMap()["new"].(float64); ok && n <= 0 {
				return &pluginv1.HandleEventResponse{}, nil
			}
		}
	}
	if fresh, err := a.store.MarkEvent(ctx, req.GetEventId()); err == nil && !fresh {
		return &pluginv1.HandleEventResponse{}, nil
	}
	settle := 5 * time.Minute
	if settings, err := a.store.Settings(ctx); err == nil {
		settle = time.Duration(settings.Delivery.SettleMinutes) * time.Minute
	}
	a.trigger(settle)
	a.log.Info("new media event", "event", req.GetEventName(), "run_in", (settle + eventBatch).String())
	return &pluginv1.HandleEventResponse{}, nil
}

type taskServer struct {
	pluginv1.UnimplementedScheduledTaskServer
	app *App
}

// Run is the scheduled task. Silo runs it at startup by default, which also
// starts the plugin process so its own timer can send digests on time.
func (s *taskServer) Run(ctx context.Context, _ *pluginv1.RunScheduledTaskRequest) (*pluginv1.RunScheduledTaskResponse, error) {
	s.app.ensureLoop()
	report := s.app.runDue(ctx, "scheduled task", true)
	errs := make([]any, 0, len(report.Errors))
	for _, e := range report.Errors {
		errs = append(errs, e)
	}
	out, _ := structpb.NewStruct(map[string]any{
		"recipients_due": report.Recipients,
		"emails_sent":    report.Sent,
		"items_found":    report.Items,
		"skipped":        report.Skipped,
		"errors":         errs,
	})
	return &pluginv1.RunScheduledTaskResponse{Output: out}, nil
}
