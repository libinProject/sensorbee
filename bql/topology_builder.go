package bql

import (
	"fmt"
	"pfi/sensorbee/sensorbee/bql/parser"
	"pfi/sensorbee/sensorbee/bql/udf"
	"pfi/sensorbee/sensorbee/core"
	"pfi/sensorbee/sensorbee/tuple"
	"sync/atomic"
)

type TopologyBuilder struct {
	topology       core.DynamicTopology
	Reg            udf.FunctionManager
	UDSCreators    udf.UDSCreatorRegistry
	SourceCreators SourceCreatorRegistry
	SinkCreators   SinkCreatorRegistry
}

// TODO: Provide AtomicTopologyBuilder which support building multiple nodes
// in an atomic manner (kind of transactionally)

// NewTopologyBuilder creates a new TopologyBuilder which dynamically creates
// nodes from BQL statements. The target DynamicTopology can be shared by
// multiple TopologyBuilders.
//
// TopologyBuilder doesn't support atomic topology building. For example,
// when a user wants to add three statement and the second statement fails,
// only the node created from the first statement is registered to the topology
// and it starts to generate tuples. Others won't be registered.
func NewTopologyBuilder(t core.DynamicTopology) (*TopologyBuilder, error) {
	udss, err := udf.CopyGlobalUDSCreatorRegistry()
	if err != nil {
		return nil, err
	}

	srcs, err := CopyGlobalSourceCreatorRegistry()
	if err != nil {
		return nil, err
	}

	sinks, err := CopyGlobalSinkCreatorRegistry()
	if err != nil {
		return nil, err
	}

	tb := &TopologyBuilder{
		topology:       t,
		Reg:            udf.CopyGlobalUDFRegistry(t.Context()),
		UDSCreators:    udss,
		SourceCreators: srcs,
		SinkCreators:   sinks,
	}
	return tb, nil
}

// TODO: if IDs are shared by distributed processes, they should have a process
// ID of each process in it or they should be generated by a central server
// to be globally unique. Currently, this id is only used temporarily and
// doesn't have to be strictly unique nor recoverable (durable).
var (
	topologyBuilderTemporaryID int64
)

func topologyBuilderNextTemporaryID() int64 {
	return atomic.AddInt64(&topologyBuilderTemporaryID, 1)
}

// AddStmt add a node created from a statement to the topology. It returns
// a created node. It returns a nil node when the statement is CREATE STATE.
func (tb *TopologyBuilder) AddStmt(stmt interface{}) (core.DynamicNode, error) {
	// TODO: Enable StopOnDisconnect properly

	// check the type of statement
	switch stmt := stmt.(type) {
	case parser.CreateSourceStmt:
		// load params into map for faster access
		paramsMap := tb.mkParamsMap(stmt.Params)

		// check if we know whis type of source
		creator, err := tb.SourceCreators.Lookup(string(stmt.Type))
		if err != nil {
			return nil, err
		}

		// if so, try to create such a source
		source, err := creator.CreateSource(tb.topology.Context(), paramsMap)
		if err != nil {
			return nil, err
		}
		return tb.topology.AddSource(string(stmt.Name), source, nil)

	case parser.CreateStreamAsSelectStmt:
		// insert a bqlBox that executes the SELECT statement
		outName := string(stmt.Name)
		box := NewBQLBox(&stmt, tb.Reg)
		// add all the referenced relations as named inputs
		dbox, err := tb.topology.AddBox(outName, box, nil)
		if err != nil {
			return nil, err
		}
		for _, rel := range stmt.Relations {
			if err := dbox.Input(rel.Name, &core.BoxInputConfig{
				InputName: rel.Name,
			}); err != nil {
				tb.topology.Remove(outName)
				return nil, err
			}
		}
		dbox.(core.DynamicBoxNode).StopOnDisconnect()
		return dbox, nil

	case parser.CreateSinkStmt:
		// load params into map for faster access
		paramsMap := tb.mkParamsMap(stmt.Params)

		// check if we know whis type of sink
		creator, err := tb.SinkCreators.Lookup(string(stmt.Type))
		if err != nil {
			return nil, err
		}

		// if so, try to create such a sink
		sink, err := creator.CreateSink(tb.topology.Context(), paramsMap)
		if err != nil {
			return nil, err
		}
		// we insert a sink, but cannot connect it to
		// any streams yet, therefore we have to keep track
		// of the SinkDeclarer
		return tb.topology.AddSink(string(stmt.Name), sink, nil)

	case parser.CreateStateStmt:
		c, err := tb.UDSCreators.Lookup(string(stmt.Type))
		if err != nil {
			return nil, err
		}

		ctx := tb.topology.Context()
		s, err := c.CreateState(ctx, tb.mkParamsMap(stmt.Params))
		if err != nil {
			return nil, err
		}
		if err := ctx.SharedStates.Add(ctx, string(stmt.Name), s); err != nil {
			return nil, err
		}
		return nil, nil

	case parser.InsertIntoSelectStmt:
		// get the sink to add an input to
		sink, err := tb.topology.Sink(string(stmt.Sink))
		if err != nil {
			return nil, err
		}
		// construct an intermediate box doing the SELECT computation.
		//   INSERT INTO sink SELECT a, b FROM c WHERE d
		// becomes
		//   CREATE STREAM (random_string) AS SELECT ISTREAM(a, b)
		//   FROM c [RANGE 1 TUPLES] WHERE d
		//  + a connection (random_string -> sink)
		tmpName := fmt.Sprintf("sensorbee_tmp_%v", topologyBuilderNextTemporaryID())
		newRels := make([]parser.AliasedStreamWindowAST, len(stmt.Relations))
		for i, from := range stmt.Relations {
			if from.Unit != parser.UnspecifiedIntervalUnit {
				return nil, fmt.Errorf("you cannot use a RANGE clause with an INSERT INTO " +
					"statement at the moment")
			} else {
				newRels[i] = from
				newRels[i].IntervalAST = parser.IntervalAST{
					parser.NumericLiteral{1}, parser.Tuples}
			}
		}
		if stmt.EmitterType != parser.UnspecifiedEmitter {
			err := fmt.Errorf("you cannot use a %s clause with an INSERT INTO "+
				"statement at the moment", stmt.EmitterType)
			return nil, err
		}
		tmpStmt := parser.CreateStreamAsSelectStmt{
			parser.StreamIdentifier(tmpName),
			parser.EmitterAST{parser.Rstream, nil},
			stmt.ProjectionsAST,
			parser.WindowedFromAST{newRels},
			stmt.FilterAST,
			stmt.GroupingAST,
			stmt.HavingAST,
		}
		box, err := tb.AddStmt(tmpStmt)
		if err != nil {
			return nil, err
		}
		box.(core.DynamicBoxNode).StopOnDisconnect()

		// now connect the sink to that box
		if err := sink.Input(tmpName, nil); err != nil {
			tb.topology.Remove(tmpName)
			return nil, err
		}
		return box, nil
	}

	return nil, fmt.Errorf("statement of type %T is unimplemented", stmt)
}

func (tb *TopologyBuilder) mkParamsMap(params []parser.SourceSinkParamAST) tuple.Map {
	paramsMap := make(tuple.Map, len(params))
	for _, kv := range params {
		paramsMap[string(kv.Key)] = kv.Value
	}
	return paramsMap
}
