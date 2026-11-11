package interbase

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"interbase-go/psql"
)

func (d *debugState) serveEvaluation(req debugEvalRequest, current *lpFrame) {
	result := debugEvalResult{}
	frame, err := debugFrameAt(current, req.frame)
	if err != nil {
		result.err = err
	} else {
		result.evaluations = make([]DebugEvaluation, len(req.expressions))
		for i, expression := range req.expressions {
			result.evaluations[i] = d.evaluateExpression(req.ctx, frame, expression)
		}
	}
	select {
	case req.result <- result:
	case <-req.ctx.Done():
	}
}

func debugFrameAt(current *lpFrame, index int) (*lpFrame, error) {
	if index < 0 {
		return nil, fmt.Errorf("interbase: debug frame index %d is out of range", index)
	}
	for i, frame := 0, current; frame != nil; i, frame = i+1, frame.caller {
		if i == index {
			return frame, nil
		}
	}
	return nil, fmt.Errorf("interbase: debug frame index %d is out of range", index)
}

func (d *debugState) evaluateExpression(ctx context.Context, frame *lpFrame, expression string) DebugEvaluation {
	text := strings.TrimSpace(expression)
	result := DebugEvaluation{Expression: text}
	if text == "" {
		result.Err = errors.New("interbase: debug expression is empty")
		return result
	}
	if strings.Contains(text, ";") {
		result.Err = errors.New("interbase: debug expressions cannot contain a semicolon")
		return result
	}
	if err := ctx.Err(); err != nil {
		result.Err = err
		return result
	}
	if frame == nil || d.run == nil || d.run.plan == nil {
		result.Err = errors.New("interbase: debug evaluation is unavailable")
		return result
	}

	names := make(map[string]bool, len(frame.vars))
	for name := range frame.vars {
		names[name] = true
	}
	if unknown := unknownColonNames(text, names); len(unknown) > 0 {
		result.Err = fmt.Errorf("interbase: debug expression references unknown variable(s): %s", strings.Join(unknown, ", "))
		return result
	}

	run := d.run
	expressionNode := psql.AnalyzeExpr(text, 1, 1, names, run.plan.dialect)
	if expressionNode.Local != nil {
		if value, ok := evalLocalValue(expressionNode.Local, frame.localScope(run.dbNone)); ok {
			if local, typeText, localOK := debugLocalResult(frame, expressionNode.Local, value); localOK {
				result.Type, result.Value = typeText, local
				return result
			}
		}
	}

	// Evaluation runs while the line-profile run goroutine is paused. Merge the
	// caller's cancellation with Stop's session cancellation for the temporary
	// server calls without changing the session's transaction.
	evalCtx, cancel := context.WithCancel(ctx)
	stopSessionCancel := context.AfterFunc(d.session.ctx, cancel)
	defer func() {
		stopSessionCancel()
		cancel()
	}()
	previousCtx := run.ctx
	run.ctx = evalCtx
	defer func() { run.ctx = previousCtx }()

	result.Type, result.Value, result.Err = evaluateServerExpression(run, frame, expressionNode)
	return result
}

func evaluateServerExpression(run *lpRun, frame *lpFrame, expression *psql.Expr) (typeText string, value any, err error) {
	statements := run.statements
	run.statements = make(map[string]cachedStatement)
	defer func() {
		err = errors.Join(err, run.closeStatements())
		if err != nil {
			typeText, value = "", nil
		}
		run.statements = statements
	}()

	query := "SELECT (" + expression.ServerText + ") FROM RDB$DATABASE"
	scratch := &LineProfileNode{ID: -1}
	statement, err := run.bindQuiet(frame, scratch, query, expression.Params, true)
	if err != nil {
		return "", nil, err
	}
	rows, err := run.queryRows(scratch, statement, 1)
	if err != nil {
		return "", nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return "", nil, errors.New("interbase: debug expression evaluation returned no value")
	}
	outputs := statement.stmt.Outputs()
	if len(outputs) != 1 {
		return "", nil, fmt.Errorf("interbase: debug expression evaluation returned %d columns, want 1", len(outputs))
	}
	value, err = run.holdValue(cellValue(rows[0][0]))
	if err != nil {
		return "", nil, err
	}
	return run.descriptorType(outputs[0]), value, nil
}

func debugLocalResult(frame *lpFrame, expression psql.LocalNode, value localValue) (any, string, bool) {
	typeText := ""
	switch value.family() {
	case famInt:
		typeText = "INTEGER"
	case famString:
		typeText = "VARCHAR"
	case famOther:
		if variable, ok := expression.(psql.LocalVar); ok {
			if declared, exists := frame.vars[variable.Name]; exists {
				typeText = declared.typ.Text
			}
		}
	}
	switch value.kind {
	case localNullKind:
		return nil, typeText, true
	case localIntKind:
		return value.i, typeText, true
	case localStringKind:
		return string(value.s), typeText, true
	case localOpaqueKind:
		if variable, ok := expression.(psql.LocalVar); ok {
			if declared, exists := frame.vars[variable.Name]; exists {
				return cloneDebugValue(declared.value), typeText, true
			}
		}
	}
	return nil, "", false
}
