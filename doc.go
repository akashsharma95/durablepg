// Package durablepg provides a durable workflow engine backed by PostgreSQL.
//
// Quick start:
//
//	engine, _ := durablepg.New(durablepg.Config{DB: pool})
//	_ = engine.Init(ctx)
//	engine.RegisterWorkflow("my_workflow", func(wf *durablepg.Builder) {})
//	go engine.StartWorker(ctx)
//	_, _ = engine.Run(ctx, "my_workflow", map[string]any{"user_id": "123"})
package durablepg
