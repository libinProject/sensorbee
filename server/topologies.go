package server

import (
	"fmt"
	"github.com/Sirupsen/logrus"
	"github.com/gocraft/web"
	"golang.org/x/net/websocket"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"pfi/sensorbee/sensorbee/bql"
	"pfi/sensorbee/sensorbee/bql/parser"
	"pfi/sensorbee/sensorbee/core"
	"pfi/sensorbee/sensorbee/data"
	"pfi/sensorbee/sensorbee/server/response"
	"strings"
	"time"
)

type topologies struct {
	*APIContext
	topologyName string
	topology     *bql.TopologyBuilder
}

func setUpTopologiesRouter(prefix string, router *web.Router) {
	root := router.Subrouter(topologies{}, "/topologies")
	root.Middleware((*topologies).extractName)
	// TODO validation (root can validate with regex like "\w+")
	root.Post("/", (*topologies).Create)
	root.Get("/", (*topologies).Index)
	root.Get(`/:topologyName`, (*topologies).Show)
	root.Delete(`/:topologyName`, (*topologies).Destroy)
	root.Post(`/:topologyName/queries`, (*topologies).Queries)
	root.Get(`/:topologyName/wsqueries`, (*topologies).WebSocketQueries)

	setUpSourcesRouter(prefix, root)
	setUpStreamsRouter(prefix, root)
	setUpSinksRouter(prefix, root)
}

func (tc *topologies) Log() *logrus.Entry {
	e := tc.APIContext.Log()
	if tc.topologyName == "" {
		return e
	}
	return e.WithField("topology", tc.topologyName)
}

func (tc *topologies) ErrLog(err error) *logrus.Entry {
	e := tc.APIContext.ErrLog(err)
	if tc.topologyName == "" {
		return e
	}
	return e.WithField("topology", tc.topologyName)
}

func (tc *topologies) extractName(rw web.ResponseWriter, req *web.Request, next web.NextMiddlewareFunc) {
	if err := tc.extractOptionStringFromPath("topologyName", &tc.topologyName); err != nil {
		return
	}
	next(rw, req)
}

// fetchTopology returns the topology having tc.topologyName. When this method
// returns nil, the caller can just return from the action.
func (tc *topologies) fetchTopology() *bql.TopologyBuilder {
	tb, err := tc.topologies.Lookup(tc.topologyName)
	if err != nil {
		if core.IsNotExist(err) {
			tc.Log().Error("The topology is not registered")
			tc.RenderErrorJSON(NewError(requestURLNotFoundErrorCode, "The topology doesn't exist",
				http.StatusNotFound, err))
			return nil
		}
		tc.ErrLog(err).WithField("err", err).Error("Cannot lookup the topology")
		tc.RenderErrorJSON(NewInternalServerError(err))
		return nil
	}
	tc.topology = tb
	return tb
}

// Create creates a new topology.
func (tc *topologies) Create(rw web.ResponseWriter, req *web.Request) {
	js, apiErr := parseJSONFromRequestBody(tc.Context)
	if apiErr != nil {
		tc.ErrLog(apiErr.Err).Error("Cannot parse the request json")
		tc.RenderErrorJSON(apiErr)
		return
	}

	// TODO: use mapstructure when parameters get too many
	form, err := data.NewMap(js)
	if err != nil {
		tc.ErrLog(err).WithField("body", js).Error("The request json may contain invalid value")
		tc.RenderErrorJSON(NewError(formValidationErrorCode, "The request json may contain invalid values.",
			http.StatusBadRequest, err))
		return
	}

	// TODO: report validation errors at once (don't report each error separately) after adding other parameters

	n, ok := form["name"]
	if !ok {
		tc.Log().Error("The required 'name' field is missing")
		e := NewError(formValidationErrorCode, "The request body is invalid.",
			http.StatusBadRequest, nil)
		e.Meta["name"] = []string{"field is missing"}
		tc.RenderErrorJSON(e)
		return
	}
	name, err := data.AsString(n)
	if err != nil {
		tc.ErrLog(err).Error("'name' field isn't a string")
		e := NewError(formValidationErrorCode, "The request body is invalid.",
			http.StatusBadRequest, nil)
		e.Meta["name"] = []string{"value must be a string"}
		tc.RenderErrorJSON(e)
		return
	}
	if err := core.ValidateNodeName(name); err != nil {
		tc.ErrLog(err).Error("'name' field has invalid format")
		e := NewError(formValidationErrorCode, "The request body is invalid.",
			http.StatusBadRequest, nil)
		e.Meta["name"] = []string{"inavlid format"}
		tc.RenderErrorJSON(e)
		return
	}

	// TODO: support other parameters

	cc := &core.ContextConfig{
		Logger: tc.logger,
	}
	// TODO: Be careful of race conditions on these fields.
	cc.Flags.DroppedTupleLog.Set(tc.config.Logging.LogDroppedTuples)
	cc.Flags.DroppedTupleSummarization.Set(tc.config.Logging.SummarizeDroppedTuples)

	tp := core.NewDefaultTopology(core.NewContext(cc), name)
	tb, err := bql.NewTopologyBuilder(tp)
	if err != nil {
		tc.ErrLog(err).Error("Cannot create a new topology builder")
		tc.RenderErrorJSON(NewInternalServerError(err))
		return
	}
	tb.UDSStorage = tc.udsStorage

	if err := tc.topologies.Register(name, tb); err != nil {
		if err := tp.Stop(); err != nil {
			tc.ErrLog(err).Error("Cannot stop the created topology")
		}

		if os.IsExist(err) {
			tc.Log().Error("the name is already registered")
			e := NewError(formValidationErrorCode, "The request body is invalid.",
				http.StatusBadRequest, nil)
			e.Meta["name"] = []string{"already taken"}
			tc.RenderErrorJSON(e)
			return
		}
		tc.ErrLog(err).WithField("err", err).Error("Cannot register the topology")
		tc.RenderJSON(NewInternalServerError(err))
		return
	}

	// TODO: return 201
	tc.RenderJSON(map[string]interface{}{
		"topology": response.NewTopology(tb.Topology()),
	})
}

// Index returned a list of registered topologies.
func (tc *topologies) Index(rw web.ResponseWriter, req *web.Request) {
	ts, err := tc.topologies.List()
	if err != nil {
		tc.ErrLog(err).Error("Cannot list registered topologies")
		tc.RenderErrorJSON(NewInternalServerError(err))
		return
	}

	res := []*response.Topology{}
	for _, tb := range ts {
		res = append(res, response.NewTopology(tb.Topology()))
	}
	tc.RenderJSON(map[string]interface{}{
		"topologies": res,
	})
}

// Show returns the information of topology
func (tc *topologies) Show(rw web.ResponseWriter, req *web.Request) {
	tb := tc.fetchTopology()
	if tb == nil {
		return
	}
	tc.RenderJSON(map[string]interface{}{
		"topology": response.NewTopology(tb.Topology()),
	})
}

// TODO: provide Update action (change state of the topology, etc.)

func (tc *topologies) Destroy(rw web.ResponseWriter, req *web.Request) {
	tb, err := tc.topologies.Unregister(tc.topologyName)
	isNotExist := core.IsNotExist(err)
	if err != nil && !isNotExist {
		tc.ErrLog(err).Error("Cannot unregister the topology")
		tc.RenderErrorJSON(NewInternalServerError(err))
		return
	}
	stopped := true
	if tb != nil {
		if err := tb.Topology().Stop(); err != nil {
			stopped = false
			tc.ErrLog(err).Error("Cannot stop the topology")
		}
	}

	if stopped {
		// TODO: return 204 when the topology didn't exist.
		tc.RenderJSON(map[string]interface{}{})
	} else {
		tc.RenderJSON(map[string]interface{}{
			"warning": map[string]interface{}{
				"message": "the topology wasn't stopped correctly",
			},
		})
	}
}

func (tc *topologies) Queries(rw web.ResponseWriter, req *web.Request) {
	tb := tc.fetchTopology()
	if tb == nil {
		return
	}

	js, apiErr := parseJSONFromRequestBody(tc.Context)
	if apiErr != nil {
		tc.ErrLog(apiErr.Err).Error("Cannot parse the request json")
		tc.RenderErrorJSON(apiErr)
		return
	}

	form, err := data.NewMap(js)
	if err != nil {
		tc.ErrLog(err).WithField("body", js).
			Error("The request json may contain invalid value")
		tc.RenderErrorJSON(NewError(formValidationErrorCode, "The request json may contain invalid values.",
			http.StatusBadRequest, err))
		return
	}

	var stmts []interface{}
	if ss, err := tc.parseQueries(form); err != nil {
		tc.RenderErrorJSON(err)
		return
	} else if len(ss) == 0 {
		// TODO: support the new format
		tc.RenderJSON(map[string]interface{}{
			"topology_name": tc.topologyName,
			"status":        "running",
			"queries":       []interface{}{},
		})
		return
	} else {
		stmts = ss
	}

	if len(stmts) == 1 {
		stmtStr := fmt.Sprint(stmts[0])
		if stmt, ok := stmts[0].(parser.SelectStmt); ok {
			tc.handleSelectStmt(rw, stmt, stmtStr)
			return
		} else if stmt, ok := stmts[0].(parser.SelectUnionStmt); ok {
			tc.handleSelectUnionStmt(rw, stmt, stmtStr)
			return
		} else if stmt, ok := stmts[0].(parser.EvalStmt); ok {
			tc.handleEvalStmt(rw, stmt, stmtStr)
			return
		}
	}

	// TODO: handle this atomically
	for _, stmt := range stmts {
		// TODO: change the return value of AddStmt to support the new response format.
		_, err := tb.AddStmt(stmt)
		if err != nil {
			tc.ErrLog(err).Error("Cannot process a statement")
			e := NewError(bqlStmtProcessingErrorCode, "Cannot process a statement", http.StatusBadRequest, err)
			e.Meta["error"] = err.Error()
			e.Meta["statement"] = fmt.Sprint(stmt)
			tc.RenderErrorJSON(e)
			return
		}
	}

	// TODO: support the new format
	tc.RenderJSON(map[string]interface{}{
		"topology_name": tc.topologyName,
		"status":        "running",
		"queries":       stmts,
	})
}

// WebSocketQueries handles requests using WebSocket. A single WebSocket
// connection can concurrently issue multiple requests including requests
// containing a SELECT statement.
//
// All WebSocket request need to have following fields:
//
//	* rid
//	* payload
//
// "rid" field is used at the client side to identify to which request a response
// corresponds. All responses have "rid" field having the same value as the one
// in its corresponding request. rid can be any number as long as the client can
// distinguish responses.
//
// "payload" field contains a request data same as the one sent to the regular
// HTTP request. Therefore, WebSocket requests have the same limitations such as
// "A SELECT statement cannot be issued with other statements including another
// SELECT statement". However, as it's mentioned earlier, a single WebSocket
// connection can concurrently send multiple requests which have a single
// SELECT statement.
//
// Example:
//
//	{
//		"rid": 1,
//		"payload": {
//			"queries": "SELECT RSTREAM * FROM my_stream [RANGE 1 TUPLES];"
//		}
//	}
//
// All WebSocket responses have following fields:
//
//	* rid
//	* type
//	* payload
//
// "rid" field contains the ID of the request to which the response corresponds.
//
// "type" field contains the type of the response:
//
//	* "result"
//	* "error"
//	* "sos"
//	* "ping"
//	* "eos"
//
// When the type is "result", "payload" field contains the result obtained by
// executing the query. The form of response depends on the type of a statement
// and some statements returns multiple responses. When the type is "error",
// "payload" has an error information which is same as the error response
// that Queries action returns. "sos", start of stream, type is used by SELECT
// statements to notify the client that a SELECT statement finishes setting up
// all necessary nodes in the topology. Its payload is always null. "ping"
// type is used by SELECT statements to validate connection. Its "payload" is
// always null. SELECT statements send "ping" responses on a regular basis.
// "eos", end of stream, responses are sent when SELECT statements has sent all
// tuples. "payload" of "eos" is always null. "eos" isn't sent when an error
// occurred.
func (tc *topologies) WebSocketQueries(rw web.ResponseWriter, req *web.Request) {
	// TODO: add a document describing which BQL statement returns which result.
	if !strings.EqualFold(req.Header.Get("Upgrade"), "WebSocket") {
		err := fmt.Errorf("the request isn't a WebSocket request")
		tc.Log().Error(err)
		tc.RenderErrorJSON(NewError(nonWebSocketRequestErrorCode, "This action only accepts WebSocket connections",
			http.StatusBadRequest, err))
		return
	}

	tb := tc.fetchTopology()
	if tb == nil {
		return
	}

	tc.Log().Info("Begin WebSocket connection")
	defer tc.Log().Info("End WebSocket connection")

	websocket.Handler(func(conn *websocket.Conn) {
		sendErr := func(e *Error) {
			err := websocket.JSON.Send(conn, e)
			if err != nil {
				tc.ErrLog(err).Error("Cannot send an error response to the WebSocket connection")
			}
		}
		var js map[string]interface{}
		if err := websocket.JSON.Receive(conn, &js); err != nil {
			e := NewError(bqlStmtParseErrorCode,
				"Cannot read or parse a JSON body received from the WebSocket connection",
				http.StatusBadRequest, err)
			tc.ErrLog(err).Error(e.Message)
			sendErr(e)
			return
		}

		form, err := data.NewMap(js)
		if err != nil {
			tc.ErrLog(err).WithField("body", js).Error("The request json may contain invalid value")
			e := NewError(formValidationErrorCode, "The request json may contain invalid values.",
				http.StatusBadRequest, err)
			sendErr(e)
			return
		}

		// TODO: use mapstructure or json schema for validation
		// TODO: return as many errors at once as possible
		var (
			rid     int64
			payload data.Map
		)
		if v, ok := form["rid"]; !ok {
			tc.Log().Error("The required 'rid' field is missing")
			e := NewError(formValidationErrorCode, "The request body is invalid.",
				http.StatusBadRequest, err)
			e.Meta["rid"] = []string{"field is missing"}
			sendErr(e)
			return

		} else if r, err := data.ToInt(v); err != nil {
			tc.ErrLog(err).Error("Cannot convert 'rid' to an integer")
			e := NewError(formValidationErrorCode, "The request body is invalid.",
				http.StatusBadRequest, err)
			e.Meta["rid"] = []string{"value must be an integer"}
			sendErr(e)
			return

		} else {
			rid = r
		}

		if v, ok := form["payload"]; !ok {
			tc.Log().Error("The required 'payload' field is missing")
			e := NewError(formValidationErrorCode, "The request body is invalid.",
				http.StatusBadRequest, err)
			e.Meta["payload"] = []string{"field is missing"}
			sendErr(e)
			return

		} else if p, err := data.AsMap(v); err != nil {
			tc.ErrLog(err).Error("Cannot convert 'payload' to an integer")
			e := NewError(formValidationErrorCode, "The request body is invalid.",
				http.StatusBadRequest, err)
			e.Meta["payload"] = []string{"value must be an object"}
			sendErr(e)
			return

		} else {
			payload = p
		}

		// TODO: merge the following implementation with Queries.
		var stmts []interface{}
		if ss, err := tc.parseQueries(payload); err != nil {
			sendErr(err)
			return
		} else if len(ss) == 0 {
			if err := websocket.JSON.Send(conn, map[string]interface{}{}); err != nil {
				tc.ErrLog(err).Error("Cannot send a response to the WebSocket client")
			}
			return
		} else {
			stmts = ss
		}

		if len(stmts) == 1 {
			stmtStr := fmt.Sprint(stmts[0])
			if stmt, ok := stmts[0].(parser.SelectStmt); ok {
				tc.handleSelectStmtWebSocket(conn, rid, stmt, stmtStr)
				return
			} else if stmt, ok := stmts[0].(parser.SelectUnionStmt); ok {
				tc.handleSelectUnionStmtWebSocket(conn, rid, stmt, stmtStr)
				return
			} else if stmt, ok := stmts[0].(parser.EvalStmt); ok {
				tc.handleEvalStmtWebSocket(conn, rid, stmt, stmtStr)
				return
			}
		}

		// TODO: handle this atomically
		for _, stmt := range stmts {
			// TODO: change the return value of AddStmt to support the new response format.
			_, err = tb.AddStmt(stmt)
			if err != nil {
				tc.ErrLog(err).Error("Cannot process a statement")
				e := NewError(bqlStmtProcessingErrorCode, "Cannot process a statement", http.StatusBadRequest, err)
				e.Meta["error"] = err.Error()
				e.Meta["statement"] = fmt.Sprint(stmt)
				sendErr(e)
				return
			}
		}

		// TODO: define a proper response format
		if err := websocket.JSON.Send(conn, map[string]interface{}{}); err != nil {
			tc.ErrLog(err).Error("Cannot send a response to the WebSocket client")
		}
	}).ServeHTTP(rw, req.Request)
}

func (tc *topologies) parseQueries(form data.Map) ([]interface{}, *Error) {
	// TODO: use mapstructure when parameters get too many
	var queries string
	if v, ok := form["queries"]; !ok {
		errMsg := "The request json doesn't have 'queries' field"
		tc.Log().Error(errMsg)
		e := NewError(formValidationErrorCode, "'queries' field is missing",
			http.StatusBadRequest, nil)
		return nil, e
	} else if f, err := data.AsString(v); err != nil {
		errMsg := "'queries' must be a string"
		tc.ErrLog(err).Error(errMsg)
		e := NewError(formValidationErrorCode, "'queries' field must be a string",
			http.StatusBadRequest, err)
		return nil, e
	} else {
		queries = f
	}

	bp := parser.New()
	stmts := []interface{}{}
	dataReturningStmtIndex := -1
	for queries != "" {
		stmt, rest, err := bp.ParseStmt(queries)
		if err != nil {
			tc.Log().WithField("parse_errors", err.Error()).
				WithField("statement", queries).Error("Cannot parse a statement")
			e := NewError(bqlStmtParseErrorCode, "Cannot parse a BQL statement", http.StatusBadRequest, err)
			e.Meta["parse_errors"] = strings.Split(err.Error(), "\n") // FIXME: too ad hoc
			e.Meta["statement"] = queries
			return nil, e
		}
		if _, ok := stmt.(parser.SelectStmt); ok {
			dataReturningStmtIndex = len(stmts)
		} else if _, ok := stmt.(parser.SelectUnionStmt); ok {
			dataReturningStmtIndex = len(stmts)
		} else if _, ok := stmt.(parser.EvalStmt); ok {
			dataReturningStmtIndex = len(stmts)
		}

		stmts = append(stmts, stmt)
		queries = rest
	}

	if dataReturningStmtIndex >= 0 {
		if len(stmts) != 1 {
			errMsg := "A SELECT or EVAL statement cannot be issued with other statements"
			tc.Log().Error(errMsg)
			e := NewError(bqlStmtProcessingErrorCode, "Cannot process a statement", http.StatusBadRequest, nil)
			e.Meta["error"] = "a SELECT or EVAL statement cannot be issued with other statements"
			e.Meta["statement"] = fmt.Sprint(stmts[dataReturningStmtIndex])
			return nil, e
		}
	}
	return stmts, nil
}

func (tc *topologies) handleSelectStmt(rw web.ResponseWriter, stmt parser.SelectStmt, stmtStr string) {
	tmpStmt := parser.SelectUnionStmt{[]parser.SelectStmt{stmt}}
	tc.handleSelectUnionStmt(rw, tmpStmt, stmtStr)
}

func (tc *topologies) handleSelectUnionStmt(rw web.ResponseWriter, stmt parser.SelectUnionStmt, stmtStr string) {
	tb := tc.fetchTopology()
	if tb == nil { // just in case
		return
	}

	sn, ch, err := tb.AddSelectUnionStmt(&stmt)
	if err != nil {
		tc.ErrLog(err).Error("Cannot process a statement")
		e := NewError(bqlStmtProcessingErrorCode, "Cannot process a statement", http.StatusBadRequest, err)
		e.Meta["error"] = err.Error()
		e.Meta["statement"] = stmtStr
		tc.RenderErrorJSON(e)
		return
	}
	defer func() {
		go func() {
			// vacuum all tuples to avoid blocking the sink.
			for _ = range ch {
			}
		}()
		if err := sn.Stop(); err != nil {
			tc.ErrLog(err).WithFields(logrus.Fields{
				"node_type": core.NTSink,
				"node_name": sn.Name(),
			}).Error("Cannot stop the temporary sink")
		}
	}()

	conn, bufrw, err := rw.Hijack()
	if err != nil {
		tc.ErrLog(err).Error("Cannot hijack a connection")
		tc.RenderErrorJSON(NewInternalServerError(err))
		return
	}

	var (
		writeErr error
		readErr  error
	)
	mw := multipart.NewWriter(bufrw)
	defer func() {
		if writeErr != nil {
			tc.ErrLog(writeErr).Info("Cannot write contents to the hijacked connection")
		}

		if err := mw.Close(); err != nil {
			if writeErr == nil && readErr == nil { // log it only when the write err hasn't happend
				tc.ErrLog(err).Info("Cannot finish the multipart response")
			}
		}
		bufrw.Flush()
		conn.Close()

		tc.Log().WithFields(logrus.Fields{
			"topology":  tc.topologyName,
			"statement": stmtStr,
		}).Info("Finish streaming SELECT responses")
	}()

	res := []string{
		"HTTP/1.1 200 OK",
		fmt.Sprintf(`Content-Type: multipart/mixed; boundary="%v"`, mw.Boundary()),
		"\r\n",
	}
	if _, err := bufrw.WriteString(strings.Join(res, "\r\n")); err != nil {
		tc.ErrLog(err).Error("Cannot write a header to the hijacked connection")
		return
	}
	bufrw.Flush()

	tc.Log().WithFields(logrus.Fields{
		"topology":  tc.topologyName,
		"statement": stmtStr,
	}).Info("Start streaming SELECT responses")

	// All error reporting logs after this is info level because they might be
	// caused by the client closing the connection.
	header := textproto.MIMEHeader{}
	header.Add("Content-Type", "application/json")

	readPoll := time.After(1 * time.Minute)
	sent := false
	dummyReadBuf := make([]byte, 1024)
	for {
		var t *core.Tuple
		select {
		case v, ok := <-ch:
			if !ok {
				return
			}
			t = v
			sent = true
		case <-readPoll:
			if sent {
				sent = false
				readPoll = time.After(1 * time.Minute)
				continue
			}

			// Assuming there's no more data to be read. Because no tuple was
			// written for past 1 minute, blocking read for 1ms here isn't a
			// big deal.
			// TODO: is there any better way to detect disconnection?
			// TODO: If general errors are checked before checking the deadline,
			//       this code doesn't have to add 1ms.
			if err := conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond)); err != nil {
				tc.ErrLog(err).Error("Cannot check the status of connection due to the failure of conn.SetReadDeadline. Stopping streaming.")
				// This isn't handled as a read error because some operating
				// systems don't support SetReadDeadline.
				return
			}
			if _, err := bufrw.Read(dummyReadBuf); err != nil {
				type timeout interface {
					Timeout() bool
				}
				if e, ok := err.(timeout); !ok || !e.Timeout() {
					// Something happend on this connection.
					readErr = err
					tc.ErrLog(err).Error("The connection may be closed from the client side")
					return
				}
			}
			readPoll = time.After(1 * time.Minute)
			continue
		}

		js := t.Data.String()
		// TODO: don't forget to convert \n to \r\n when returning
		// pretty-printed JSON objects.
		header.Set("Content-Length", fmt.Sprint(len(js)))

		w, err := mw.CreatePart(header)
		if err != nil {
			writeErr = err
			return
		}
		if _, err := io.WriteString(w, js); err != nil {
			writeErr = err
			return
		}
		if err := bufrw.Flush(); err != nil {
			writeErr = err
			return
		}
	}
}

func (tc *topologies) handleEvalStmt(rw web.ResponseWriter, stmt parser.EvalStmt, stmtStr string) {
	tb := tc.fetchTopology()
	if tb == nil { // just in case
		return
	}

	result, err := tb.RunEvalStmt(&stmt)
	if err != nil {
		tc.ErrLog(err).Error("Cannot process a statement")
		e := NewError(bqlStmtProcessingErrorCode, "Cannot process a statement", http.StatusBadRequest, err)
		e.Meta["error"] = err.Error()
		e.Meta["statement"] = stmtStr
		tc.RenderErrorJSON(e)
		return
	}

	// return value with JSON wrapper so it can be parsed on the client side
	tc.RenderJSON(map[string]interface{}{
		"result": result,
	})
}

func (tc *topologies) handleSelectStmtWebSocket(conn *websocket.Conn, rid int64, stmt parser.SelectStmt, stmtStr string) {
	tmpStmt := parser.SelectUnionStmt{[]parser.SelectStmt{stmt}}
	tc.handleSelectUnionStmtWebSocket(conn, rid, tmpStmt, stmtStr)
}

func (tc *topologies) handleSelectUnionStmtWebSocket(conn *websocket.Conn, rid int64, stmt parser.SelectUnionStmt, stmtStr string) {
	// TODO: merge this function with handleSelectUnionStmt if possible
	tb := tc.fetchTopology()
	if tb == nil { // just in case
		return
	}

	sn, ch, err := tb.AddSelectUnionStmt(&stmt)
	if err != nil {
		tc.ErrLog(err).Error("Cannot process a statement")
		e := NewError(bqlStmtProcessingErrorCode, "Cannot process a statement", http.StatusBadRequest, err)
		e.Meta["error"] = err.Error()
		e.Meta["statement"] = stmtStr
		if err := websocket.JSON.Send(conn, map[string]interface{}{
			"rid":     rid,
			"type":    "error",
			"payload": e,
		}); err != nil {
			tc.ErrLog(err).Error("Cannot send an error response to the WebSocket client")
			return
		}
		return
	}
	defer func() {
		tc.Log().WithFields(logrus.Fields{
			"topology":  tc.topologyName,
			"statement": stmtStr,
		}).Info("Finish streaming SELECT responses")

		go func() {
			// vacuum all tuples to avoid blocking the sink.
			for _ = range ch {
			}
		}()
		if err := sn.Stop(); err != nil {
			tc.ErrLog(err).WithFields(logrus.Fields{
				"node_type": core.NTSink,
				"node_name": sn.Name(),
			}).Error("Cannot stop the temporary sink")
		}
	}()

	tc.Log().WithFields(logrus.Fields{
		"topology":  tc.topologyName,
		"statement": stmtStr,
	}).Info("Start streaming SELECT responses")

	if err := websocket.JSON.Send(conn, map[string]interface{}{
		"rid":     rid,
		"type":    "sos",
		"payload": nil,
	}); err != nil {
		tc.ErrLog(err).Error("Cannot send an sos to the WebSocket client")
		return
	}

	ping := time.After(1 * time.Minute)
	sent := false
	for {
		var t *core.Tuple
		select {
		case v, ok := <-ch:
			if !ok {
				if err := websocket.JSON.Send(conn, map[string]interface{}{
					"rid":     rid,
					"type":    "eos",
					"payload": nil,
				}); err != nil {
					tc.ErrLog(err).Error("Cannot send an EOS message to the WebSocket client")
				}
				return
			}
			t = v
			sent = true
		case <-ping:
			if sent {
				sent = false
				ping = time.After(1 * time.Minute)
				continue
			}

			if err := websocket.JSON.Send(conn, map[string]interface{}{
				"rid":     rid,
				"type":    "ping",
				"payload": nil,
			}); err != nil {
				tc.ErrLog(err).Error("The connection may be closed from the client side")
				return
			}
			ping = time.After(1 * time.Minute)
			continue
		}

		if err := websocket.JSON.Send(conn, map[string]interface{}{
			"rid":     rid,
			"type":    "result",
			"payload": t.Data,
		}); err != nil {
			tc.ErrLog(err).Error("Cannot send an error response to the WebSocket client")
			return
		}
	}
}

func (tc *topologies) handleEvalStmtWebSocket(conn *websocket.Conn, rid int64, stmt parser.EvalStmt, stmtStr string) {
	tb := tc.fetchTopology()
	if tb == nil { // just in case
		return
	}

	result, err := tb.RunEvalStmt(&stmt)
	if err != nil {
		tc.ErrLog(err).Error("Cannot process a statement")
		e := NewError(bqlStmtProcessingErrorCode, "Cannot process a statement", http.StatusBadRequest, err)
		e.Meta["error"] = err.Error()
		e.Meta["statement"] = stmtStr
		if err := websocket.JSON.Send(conn, map[string]interface{}{
			"rid":     rid,
			"type":    "error",
			"payload": e,
		}); err != nil {
			tc.ErrLog(err).Error("Cannot send an error response to the WebSocket client")
		}
		return
	}

	if err := websocket.JSON.Send(conn, map[string]interface{}{
		"rid":  rid,
		"type": "result",
		"payload": map[string]interface{}{
			"result": result,
		},
	}); err != nil {
		tc.ErrLog(err).Error("Cannot send data to the WebSocket client")
		return
	}
}
