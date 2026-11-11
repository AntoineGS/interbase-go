package interbase

import (
	"reflect"

	"interbase-go/psql"
)

func buildDebugSnapshot(d *debugState, current *lpFrame, node *psql.Node, status DebugStatus) DebugSnapshot {
	s := d.session
	s.mu.Lock()
	defer s.mu.Unlock()
	result := DebugSnapshot{
		Status: status, Phase: d.phase, Mixed: d.mixed,
		Statements: d.statements,
		Messages:   append([]DebugMessage(nil), d.messages...),
	}
	if d.run != nil && d.run.result != nil {
		result.Columns = append([]string(nil), d.run.result.Columns...)
		result.Rows = cloneRows(d.run.result.KeptRows)
		result.RowsTotal = d.run.result.Rows
	}
	for frame := current; frame != nil; frame = frame.caller {
		frameNode := frame.current
		if frame == current {
			frameNode = node
		}
		result.Frames = append(result.Frames, buildDebugFrame(d, frame, frameNode))
	}
	return result
}

func buildDebugFrame(d *debugState, frame *lpFrame, node *psql.Node) DebugFrame {
	out := DebugFrame{Routine: frame.meta.name}
	if node != nil {
		out.Kind = string(node.Kind)
		out.Line, out.Column = node.Line, node.Column
		out.EndLine, out.EndColumn = node.EndLine, node.EndColumn
	}
	values := make(map[string]any, len(frame.vars))
	appendValue := func(target *[]DebugValue, parameter lpParam) {
		variable := frame.vars[parameter.name]
		if variable == nil {
			return
		}
		value := cloneDebugValue(variable.value)
		previous, found := d.previous[frame.id][parameter.name]
		changed := !found || !reflect.DeepEqual(previous, value)
		*target = append(*target, DebugValue{
			Name: parameter.name, Type: parameter.typeText, Value: value, Changed: changed,
		})
		values[parameter.name] = value
	}
	for _, parameter := range frame.meta.inputs {
		appendValue(&out.Inputs, parameter)
	}
	for _, parameter := range frame.meta.outputs {
		appendValue(&out.Outputs, parameter)
	}
	if frame.meta.routine != nil {
		for _, declaration := range frame.meta.routine.Declarations {
			appendValue(&out.Locals, lpParam{name: declaration.Name, typeText: declaration.TypeText})
		}
	}
	d.previous[frame.id] = values
	for _, cursor := range frame.cursors {
		out.Cursors = append(out.Cursors, DebugCursor{
			Line: cursor.line, Fetched: cursor.fetched,
			Targets: append([]string(nil), cursor.targets...),
		})
	}
	return out
}

func cloneDebugValue(value any) any {
	switch value := value.(type) {
	case []byte:
		return append([]byte(nil), value...)
	default:
		return value
	}
}

func cloneRows(rows [][]Cell) [][]Cell {
	if rows == nil {
		return nil
	}
	cloned := make([][]Cell, len(rows))
	for i, row := range rows {
		cloned[i] = append([]Cell(nil), row...)
		for j := range cloned[i] {
			cloned[i][j].Value = cloneDebugValue(cloned[i][j].Value)
		}
	}
	return cloned
}

func cloneDebugSnapshot(snapshot DebugSnapshot) DebugSnapshot {
	snapshot.Frames = append([]DebugFrame(nil), snapshot.Frames...)
	for i := range snapshot.Frames {
		frame := &snapshot.Frames[i]
		frame.Inputs = cloneDebugValues(frame.Inputs)
		frame.Outputs = cloneDebugValues(frame.Outputs)
		frame.Locals = cloneDebugValues(frame.Locals)
		frame.Cursors = append([]DebugCursor(nil), frame.Cursors...)
		for j := range frame.Cursors {
			frame.Cursors[j].Targets = append([]string(nil), frame.Cursors[j].Targets...)
		}
	}
	snapshot.Columns = append([]string(nil), snapshot.Columns...)
	snapshot.Rows = cloneRows(snapshot.Rows)
	snapshot.Messages = append([]DebugMessage(nil), snapshot.Messages...)
	if snapshot.FailedAt != nil {
		location := *snapshot.FailedAt
		snapshot.FailedAt = &location
	}
	return snapshot
}

func cloneDebugValues(values []DebugValue) []DebugValue {
	cloned := append([]DebugValue(nil), values...)
	for i := range cloned {
		cloned[i].Value = cloneDebugValue(cloned[i].Value)
	}
	return cloned
}
