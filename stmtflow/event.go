package stmtflow

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/zyguan/sqlz/resultset"
)

const (
	EventBlock  = "Block"
	EventResume = "Resume"
	EventInvoke = "Invoke"
	EventReturn = "Return"
)

func NewBlockEvent(s string) Event {
	return Event{EventMeta: EventMeta{EventBlock, s}}
}

func NewResumeEvent(s string) Event {
	return Event{EventMeta: EventMeta{EventResume, s}}
}

func NewInvokeEvent(s string, inv Invoke) Event {
	return Event{EventMeta: EventMeta{EventInvoke, s}, inv: &inv}
}

func NewReturnEvent(s string, ret Return) Event {
	return Event{EventMeta: EventMeta{EventReturn, s}, ret: &ret}
}

type EventMeta struct {
	Kind    string `json:"kind"`
	Session string `json:"session"`
}

func (e EventMeta) String() string {
	return e.Session + ":" + strings.ToLower(e.Kind)
}

type Event struct {
	EventMeta
	inv *Invoke
	ret *Return
}

type eventInvoke struct {
	EventMeta
	Stmt Stmt `json:"stmt"`
}

type eventReturn struct {
	EventMeta
	Stmt   Stmt            `json:"stmt"`
	T      []int64         `json:"t"`
	Data   [][]interface{} `json:"data,omitempty"`
	Result *string         `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

func (e Event) MarshalJSON() ([]byte, error) {
	switch e.Kind {
	case EventBlock, EventResume:
		return json.Marshal(e.EventMeta)
	case EventInvoke:
		inv := eventReturn{EventMeta: e.EventMeta}
		if e.inv == nil {
			return nil, errors.New("invoke data is missing")
		}
		inv.Stmt = e.inv.Stmt
		return json.Marshal(inv)
	case EventReturn:
		ret := eventReturn{EventMeta: e.EventMeta}
		if e.ret == nil {
			return nil, errors.New("return data is missing")
		}
		ret.Stmt = e.ret.Stmt
		ret.T = []int64{e.ret.T[0].UnixNano(), e.ret.T[1].UnixNano()}
		if err := e.ret.Err; err != nil {
			ret.Error = WrapError(err).(*Error)
			return json.Marshal(ret)
		}
		rs := e.ret.Res
		raw, err := rs.Encode()
		if err != nil {
			return nil, err
		}
		s := base64.StdEncoding.EncodeToString(raw)
		ret.Result = &s
		if !e.ret.Res.IsExecResult() {
			rows, cols := rs.NRows(), rs.NCols()
			mem := make([]interface{}, rows*cols)
			for i := 0; i < rows; i++ {
				for j := 0; j < cols; j++ {
					if x, ok := rs.RawValue(i, j); ok && x != nil {
						mem[i*cols+j] = string(x)
					}
				}
				ret.Data = append(ret.Data, mem[i*cols:(i+1)*cols])
			}
		}
		return json.Marshal(ret)
	default:
		return nil, errors.New("unknown event: " + e.Kind)
	}
}

func (e *Event) UnmarshalJSON(data []byte) error {
	var meta EventMeta
	err := json.Unmarshal(data, &meta)
	if err != nil {
		return err
	}
	e.EventMeta = meta
	switch e.Kind {
	case EventBlock, EventResume:
		return nil
	case EventInvoke:
		var inv eventInvoke
		if err = json.Unmarshal(data, &inv); err != nil {
			return err
		}
		e.inv = &Invoke{Stmt: inv.Stmt}
		return nil
	case EventReturn:
		var ret eventReturn
		if err = json.Unmarshal(data, &ret); err != nil {
			return err
		}
		e.ret = &Return{}
		e.ret.Stmt = ret.Stmt
		if len(ret.T) > 0 {
			e.ret.T[0] = time.Unix(0, ret.T[0])
		}
		if len(ret.T) > 1 {
			e.ret.T[1] = time.Unix(0, ret.T[1])
		}
		if ret.Error != nil {
			e.ret.Err = ret.Error
			return nil
		}
		if ret.Result == nil {
			return errors.New("invalid return event: `error` or `result` is missing")
		}
		raw, err := base64.StdEncoding.DecodeString(*ret.Result)
		if err != nil {
			return err
		}
		e.ret.Res = new(resultset.ResultSet)
		return e.ret.Res.Decode(raw)
	default:
		return errors.New("unknown event: " + e.Kind)
	}
}

func (e *Event) EqualTo(other Event, opts ...resultset.DigestOptions) (bool, string) {
	if e.EventMeta != other.EventMeta {
		return false, fmt.Sprintf("expect %+v, got %+v", e.EventMeta, other.EventMeta)
	}
	tag := e.EventMeta.String()
	if e.Kind == EventInvoke {
		thisInv, thatInv := e.Invoke(), other.Invoke()
		tag += "(" + thisInv.Stmt.SQL + ")"
		if thisInv.Stmt != thatInv.Stmt {
			return false, fmt.Sprintf(tag+": expect %+v, got %+v", thisInv.Stmt, thatInv.Stmt)
		}
	} else if e.Kind == EventReturn {
		thisRet, thatRet := e.Return(), other.Return()
		tag += "(" + thisRet.Stmt.SQL + ")"
		if thisRet.Stmt != thatRet.Stmt {
			return false, fmt.Sprintf(tag+": expect %+v, got %+v", thisRet.Stmt, thatRet.Stmt)
		}
		if thisRet.Err != nil {
			if thatRet.Err == nil {
				return false, fmt.Sprintf(tag+": expect (%s), got ok", thisRet.Err.Error())
			}
			e1, e2 := WrapError(thisRet.Err).(*Error), WrapError(thatRet.Err).(*Error)
			if e1.Code != e2.Code || (e1.Code < 0 && e1.Message != e2.Message) {
				return false, fmt.Sprintf(tag+": expect (%s), got (%s)", e1.Error(), e2.Error())
			}
		} else {
			if thatRet.Res == nil {
				return false, fmt.Sprintf(tag+": expect a result, got (%s)", thatRet.Err.Error())
			}
			r1, r2 := thisRet.Res, thatRet.Res
			if r1.IsExecResult() != r2.IsExecResult() {
				return false, fmt.Sprintf(tag+": expect [%s], got [%s]", r1, r2)
			}
			if !r1.IsExecResult() {
				var o resultset.DigestOptions
				if len(opts) > 0 {
					o = opts[0]
				}
				h1, h2 := "", ""
				o.Sort = o.Sort || thisRet.Stmt.Flags&S_UNORDERED > 0
				h1 = r1.DataDigest(o)
				h2 = r2.DataDigest(o)
				if h1 != h2 {
					return false, fmt.Sprintf(tag+": expect digest %s, got %s", h1, h2)
				}
			}
		}
	}
	return true, ""
}

func (e *Event) Invoke() Invoke { return *e.inv }

func (e *Event) Return() Return { return *e.ret }

func (e *Event) DumpText(w io.Writer, opts TextDumpOptions) {
	switch e.Kind {
	case EventInvoke:
		sql := e.Invoke().SQL
		if !strings.HasPrefix(sql, "/*") {
			sql = fmt.Sprintf("/* %s */ %s", e.Invoke().Sess, sql)
		}
		fmt.Fprintln(w, sql)
	case EventReturn:
		ret := e.Return()
		if ret.Err == nil {
			if opts.Verbose && !ret.Res.IsExecResult() {
				buf, fst := new(bytes.Buffer), true
				ret.Res.PrettyPrint(buf)
				for {
					line, err := buf.ReadString('\n')
					if err != nil {
						break
					}
					if fst {
						fmt.Fprint(w, "-- ", e.Session, " >> ", line)
						fst = false
					} else {
						fmt.Fprint(w, "-- ", e.Session, "    ", line)
					}
				}
			} else {
				fmt.Fprintf(w, "-- %s >> %s\n", e.Session, ret.Res.String())
			}
			if opts.WithLat {
				fmt.Fprintf(w, "-- %s    %s ~ %s (cost %s)\n", e.Session,
					ret.T[0].Format("15:04:05.000"), ret.T[1].Format("15:04:05.000"), ret.T[1].Sub(ret.T[0]))
			}
		} else {
			fmt.Fprintf(w, "-- %s >> %s\n", e.Session, ret.Err.Error())
		}
	case EventBlock:
		fmt.Fprintf(w, "-- %s >> blocked\n", e.Session)
	case EventResume:
		fmt.Fprintf(w, "-- %s >> resumed\n", e.Session)
	}
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e.Code == 0 {
		return e.Message
	}
	return fmt.Sprintf("E%d: %s", e.Code, e.Message)
}

func WrapError(err error) error {
	if err == nil {
		return nil
	}
	switch theErr := err.(type) {
	case *mysql.MySQLError:
		return &Error{int(theErr.Number), theErr.Message}
	case *Error:
		return theErr
	default:
		return &Error{-1, err.Error()}
	}
}

type History []Event

type JsonDumpOptions struct {
	Prefix string
	Indent string
}

func (h History) DumpJson(w io.Writer, opts JsonDumpOptions) error {
	enc := json.NewEncoder(w)
	enc.SetIndent(opts.Prefix, opts.Indent)
	return enc.Encode(h)
}

type TextDumpOptions struct {
	Verbose bool
	WithLat bool
}

func (h History) DumpText(w io.Writer, opts TextDumpOptions) error {
	for _, e := range h {
		e.DumpText(w, opts)
	}
	return nil
}

func (h *History) Collect(e Event) { *h = append(*h, e) }

func TextDumper(w io.Writer, opts TextDumpOptions) func(Event) {
	return func(e Event) {
		e.DumpText(w, opts)
	}
}

func ComposeHandler(fs ...func(Event)) func(Event) {
	return func(event Event) {
		for _, f := range fs {
			f(event)
		}
	}
}
