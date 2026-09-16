package tools

import "selfmind/internal/kernel"

// ResultTool is the typed execution seam. The registry invokes exactly one of
// ResultTool, ContextTool, or the legacy Tool, through the same policy chain.
type ResultTool interface {
	ExecuteResult(map[string]interface{}) (kernel.ToolDispatchResult, error)
}

type ResultExecutor func(map[string]interface{}) (kernel.ToolDispatchResult, error)
type ResultMiddleware func(ResultExecutor) ResultExecutor

// adaptMiddleware keeps legacy policy middleware on the single typed pipeline.
// Facts travel in this invocation's return values, never through mutable args.
// A short circuit is explicitly not invoked; rewriting output preserves facts.
func adaptMiddleware(middleware Middleware) ResultMiddleware {
	return func(next ResultExecutor) ResultExecutor {
		return func(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
			invoked := false
			result := kernel.ToolDispatchResult{Invoked: &invoked}
			output, err := middleware(func(inner map[string]interface{}) (string, error) {
				var nextErr error
				result, nextErr = next(inner)
				return result.Output, nextErr
			})(args)
			result.Output = output
			return result, err
		}
	}
}

func (r *Registry) wrapResult(t Tool, middleware []ResultMiddleware) ResultExecutor {
	exec := func(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
		var result kernel.ToolDispatchResult
		var err error
		if typed, ok := t.(ResultTool); ok {
			result, err = typed.ExecuteResult(args)
		} else if contextual, ok := t.(ContextTool); ok {
			result.Output, err = contextual.ExecuteContext(ContextFromArgs(args), args)
		} else {
			result.Output, err = t.Execute(args)
		}
		if executionPolicyForTool(t).Origin != ToolSchemaOriginBuiltin {
			// External output cannot assert local process or verification facts.
			result.Process, result.Evidence = nil, nil
		}
		invoked := true
		result.Invoked = &invoked
		return result, err
	}
	for i := len(middleware) - 1; i >= 0; i-- {
		exec = middleware[i](exec)
	}
	return func(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
		if args == nil {
			args = make(map[string]interface{})
		}
		args["_tool_name"] = t.Name()
		args["_registry"] = r
		args[toolExecutionPolicyArg] = executionPolicyForTool(t)
		if clarify := r.ClarifyHandler(); clarify != nil {
			args["_clarify_fn"] = clarify
		}
		return exec(args)
	}
}

func (r *Registry) UseResultMiddleware(middleware ResultMiddleware) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.middleware = append(r.middleware, middleware)
}

func (d *Dispatcher) InjectResultMiddleware(middleware ResultMiddleware) {
	d.registry.UseResultMiddleware(middleware)
}

func (d *Dispatcher) DispatchResult(name string, args map[string]interface{}) (kernel.ToolDispatchResult, error) {
	return d.registry.DispatchResult(name, args)
}

func processToolResult(result ExecutionResult) kernel.ToolDispatchResult {
	process := &kernel.ToolProcessResult{Started: result.Started, SandboxMode: string(result.Plan.Mode), RecoveryOutcome: result.RecoveryOutcome}
	if result.ExitCodeKnown {
		code := result.ExitCode
		process.ExitCode = &code
	}
	return kernel.ToolDispatchResult{Output: result.Output, Process: process}
}
