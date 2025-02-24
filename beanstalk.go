// Client library for the beanstalkd protocol.
// See http://kr.github.com/beanstalkd/
//
// We are lenient about the protocol -- we accept either CR LF or just LF to
// terminate server replies. We also trim white space around words in reply
// lines.
//
// This package is synchronized internally. It is safe to call any of these
// functions from any goroutine at any time.
//
// Note that, as of version 1.4.4, beanstalkd provides only 1-second
// granularity on all duration values.
package beanstalk

import (
	"bufio"
	"container/list"
	"container/vector"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// A connection to beanstalkd. Provides methods that operate outside of any
// tube. This type also embeds Tube and TubeSet, which is convenient if you
// rarely change tubes.
type Conn struct {
	Name string
	*Tube
	*TubeSet
	toSend chan<- op
}

type Job struct {
	Id uint64
	Body string
	C *Conn
}

// Represents a single tube. Provides methods that operate on one tube,
// especially Put.
type Tube struct {
	Name string
	C *Conn
}

// Represents a set of tubes. Provides methods that operate on several tubes at
// once, especially Reserve.
type TubeSet struct {
	Names []string
	UsTimeout uint64
	C *Conn
}

// Implements os.Error
type Error struct {
	ConnName string
	Cmd string
	Reply string
	Error os.Error
}

type TubeError struct {
	TubeName string
	Error os.Error
}

type op struct {
	cmd string
	tube string // For commands that depend on the used tube.
	tubes []string // For commands that depend on the watch list.
	promise chan<- result
}

type result struct {
	cmd string
	line string // The unparsed reply line.
	body string // The body, if any.
	name string // The first word of the reply line.
	args []string // The other words of the reply line.
	err os.Error // An error, if any.
}

func (e Error) String() string {
	return fmt.Sprintf("%s: %q -> %q: %s", e.ConnName, e.Cmd, e.Reply, e.Error.String());
}

func (e TubeError) String() string {
	return fmt.Sprintf("%s: %q", e.Error, e.TubeName)
}

// For use in parameters that measure duration (in microseconds). Not really
// infinite; merely large. About 126 years.
const Forever = 4000000000000000 // µs

var nameRegexp = regexp.MustCompile("^[A-Za-z0-9\\-+/;.$_()]+$")

// The server sent a bad reply. For example: unknown or inappropriate
// response, wrong number of terms, or invalid format.
var BadReply = os.NewError("Bad Reply from Server")

// Reasons for an invalid tube name.
var (
	NameTooLong = os.NewError("name too long")
	IllegalChar = os.NewError("name contains illegal char")
)

// Error responses from the server.
var (
	OutOfMemory = os.NewError("Server Out of Memory")
	InternalError = os.NewError("Server Internal Error")
	Draining = os.NewError("Server Draining")
	Buried = os.NewError("Buried")
	JobTooBig = os.NewError("Job Too Big")
	TimedOut = os.NewError("Reserve Timed Out")
	NotFound = os.NewError("Job or Tube Not Found")
	NotIgnored = os.NewError("Tube Not Ignored")

	// The server can return these but we hide them.
	deadlineSoon = os.NewError("Job Deadline Soon")

	// We entirely avoid causing these errors. They should never happen.
	badFormat = os.NewError("Bad Command Format")
	unknownCommand = os.NewError("Unknown Command")
	expectedCrLf = os.NewError("Server Expected CR LF")
)

var replyErrors = map[string]os.Error {
	"INTERNAL_ERROR": InternalError,
	"OUT_OF_MEMORY": OutOfMemory,
	"NOT_FOUND": NotFound,
	"BAD_FORMAT": badFormat,
	"UNKNOWN_COMMAND": unknownCommand,
	"BURIED": Buried,
	"DEADLINE_SOON": deadlineSoon,
}

func milliseconds(µs uint64) uint64 {
	return µs / 1000
}

func seconds(µs uint64) uint64 {
	return milliseconds(µs) / 1000
}

func push(ops []op, o op) []op {
	l := len(ops)
	if l + 1 > cap(ops) { // need to grow?
		newOps := make([]op, (l + 1) * 2) // double
		for i, o := range ops {
			newOps[i] = o
		}
		ops = newOps
	}
	ops = ops[0:l + 1] // increase the len, not the cap
	ops[l] = o
	return ops
}

// Read from toSend as many items as possible without blocking.
func collect(toSend <-chan op) (ops []op) {
	o, more := <-toSend // blocking
	
	if more {
		ops = push(ops, o)
	}
	
	//non blocking
	for {
		select {
			case o, more := <-toSend:
				if more {
					ops = push(ops, o)
				}
				break
			default:
				return
				break
		}
	}

	return
}

func (o op) resolve(line, body, name string, args []string, err os.Error) {
	go func() {
		o.promise <- result{o.cmd, line, body, name, args, err}
	}()
}

func (o op) resolveErr(line string, err os.Error) {
	o.resolve(line, "", "", []string{}, err)
}

// Optimize ops WRT the used tube.
func optUsed(tube string, ops []op) (string, []op) {
	newOps := make([]op, 0, len(ops))
	for _, o := range ops {
		if len(o.tube) > 0 {
			newTube := o.tube

			// Leave out this command and resolve its promise
			// directly.
			if newTube != tube {
				var use op
				o, use = useOp(newTube, o)
				newOps = push(newOps, use)
			}

			tube = newTube
		}
		newOps = push(newOps, o)
	}
	return tube, newOps
}

// We assume this command will succeed.
func useOp(tube string, dep op) (old, use op) {
	a := make(chan result)
	b := make(chan result)

	use.cmd = fmt.Sprintf("use %s\r\n", tube)
	use.promise = a

	old = dep
	old.promise = b

	go func () {
		r1 := <-a
		r2 := <-b

		if r2.err != nil {
			dep.promise <- r2
			return
		}

		if r1.err != nil {
			dep.promise <- r1
			return
		}

		if err, ok := replyErrors[r1.name]; ok {
			r1.err = err
			dep.promise <- r1
			return
		}

		dep.promise <- r2
	}()

	return
}

// We assume this command will succeed.
func watchOp(tube string) (o op) {
	o.cmd = fmt.Sprintf("watch %s\r\n", tube)
	o.promise = make(chan result)
	return
}

// We assume this command will succeed.
func ignoreOp(tube string) (o op) {
	o.cmd = fmt.Sprintf("ignore %s\r\n", tube)
	o.promise = make(chan result)
	return
}

// Optimize/generate ops WRT the Watch list.
func optWatched(tubes []string, ops []op) ([]string, []op) {
	tubeMap := make(map[string]bool)
	for _, s := range tubes {
		tubeMap[s] = true
	}
	newOps := make([]op, 0, len(ops))
	for _, o := range ops {
		if len(o.tubes) > 0 {
			newTubes := o.tubes
			newTubeMap := make(map[string]bool)
			for _, s := range newTubes {
				newTubeMap[s] = true
			}

			for _, s := range newTubes {
				if _, ok := tubeMap[s]; !ok {
					newOps = push(newOps, watchOp(s))
				}
			}

			for _, s := range tubes {
				if _, ok := newTubeMap[s]; !ok {
					newOps = push(newOps, ignoreOp(s))
				}
			}

			tubes = newTubes
			tubeMap = newTubeMap
		}
		newOps = push(newOps, o)
	}
	return tubes, newOps
}

// Reordering, compressing, optimization.
func prepare(ops []op) string {
	var cmds vector.StringVector
	for _, o := range ops {
		cmds.Push(o.cmd)
	}

	return strings.Join([]string(cmds), "")
}

func send(toSend <-chan op, wr io.Writer, sent chan<- op) {
	used := "default"
	watched := []string{"default"}
	for {
		ops := collect(toSend)
		used, ops = optUsed(used, ops)
		watched, ops = optWatched(watched, ops)
		cmds := prepare(ops)

		n, err := io.WriteString(wr, cmds)

		if err != nil {
			fmt.Printf("got err %s\n", err)
		}

		if n != len(cmds) {
			fmt.Printf("bad len %d != %d\n", n, len(cmds))
		}

		for _, o := range ops {
			sent <- o
		}

	}
}

func bodyLen(reply string, args []string) int {
	switch reply {
	case "FOUND", "RESERVED":
		if len(args) != 2 {
			return 0
		}
		l, err := strconv.Atoi(args[1])
		if err != nil {
			return 0
		}
		return l
	case "OK":
		if len(args) != 1 {
			return 0
		}
		l, err := strconv.Atoi(args[0])
		if err != nil {
			return 0
		}
		return l
	}
	return 0
}

func maps(f func(string) string, ss []string) (out []string) {
	out = make([]string, len(ss))
	for i, s := range ss {
		out[i] = f(s)
	}
	return out
}

func recv(raw io.Reader, ops <-chan op) {
	rd := bufio.NewReader(raw)
	for {
		// Read the next server reply.
		line, err := rd.ReadString('\n')

		if err != nil {
			(<-ops).resolveErr(line, err)
			return
		}

		split := maps(strings.TrimSpace, strings.Split(line, " "))
		reply, args := split[0], split[1:]

		// Read the body, if any.
		var body []byte
		if n := bodyLen(reply, args); n > 0 {
			body = make([]byte, n)
			r, err := io.ReadFull(rd, body)

			if err != nil {
				panic("2 todo properly teardown the Conn")
			}

			if r != n {
				panic("3 todo properly teardown the Conn")
			}
			
			//trash the trailing \r\n
			if _, err := io.ReadFull(rd, make([]byte, 2)); err != nil { 
				panic("4 todo properly teardown the Conn")
		    }
		}

		// Get the corresponding op and deliver the result.
		(<-ops).resolve(line, string(body), reply, args, nil)
	}
}

func flow(in <-chan op, out chan<- op) {
	pipeline := list.New()
	for {
		nextOut := pipeline.Front()
		if nextOut != nil {
			select {
			case nextIn := <-in:
				   pipeline.PushBack(nextIn)
			case out <- nextOut.Value.(op):
				   pipeline.Remove(nextOut)
			}
		} else {
			pipeline.PushBack(<-in)
		}
	}
}

// Simulate a buffered channel with unlimited capacity.
func bigChan() (chan<- op, <-chan op) {
	a, b := make(chan op), make(chan op)
	go flow(a, b)
	return a, b
}

// Dial the beanstalkd server at remote address addr.
func Dial(addr string) (*Conn, os.Error) {
	rw, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	return newConn(addr, rw), nil
}

// The name parameter should be descriptive. It is usually the remote address
// of the connection.
func newConn(name string, rw io.ReadWriter) *Conn {
	toSend := make(chan op)
	sentIn, sentOut := bigChan()

	go send(toSend, rw, sentIn)
	go recv(rw, sentOut)

	c := new(Conn)
	c.Name = name
	c.toSend = toSend
	c.Tube, _ = NewTube(c, "default") // This tube name is ok
	c.TubeSet, _ = NewTubeSet(c, []string{"default"}) // This tube name is ok
	return c
}

func (c *Conn) cmdWait(cmd string, tube string, tubes []string) result {
	p := make(chan result)
	c.toSend <- op{cmd, tube, tubes, p}
	return <-p
}

func (c *Conn) cmd(format string, a ...interface{}) result {
	cmd := fmt.Sprintf(format, a...)
	return c.cmdWait(cmd, "", []string{})
}

func (t Tube) cmd(format string, a ...interface{}) result {
	cmd := fmt.Sprintf(format, a...)
	return t.C.cmdWait(cmd, t.Name, []string{})
}

func (t TubeSet) cmd(format string, a ...interface{}) result {
	cmd := fmt.Sprintf(format, a...)
	return t.C.cmdWait(cmd, "", t.Names)
}

// Put a job into the queue and return its id.
//
// If an error occured, err will be non-nil. For some errors, Put will also
// return a valid job id, so you must check both values.
//
// The delay and ttr are measured in microseconds.
func (t Tube) Put(body string, pri uint32, µsDelay, µsTTR uint64) (id uint64, err os.Error) {
	r := t.cmd("put %d %d %d %d\r\n%s\r\n", pri, seconds(µsDelay), seconds(µsTTR), len(body), body)
	return r.checkForInt(t.C, "INSERTED")
}

func (r result) checkForJob(c *Conn, s string) (*Job, os.Error) {
	if r.err != nil {
		return nil, Error{c.Name, r.cmd, r.line, r.err}
	}

	if err, ok := replyErrors[r.name]; ok {
		return nil, Error{c.Name, r.cmd, r.line, err}
	}

	if r.name != s {
		return nil, Error{c.Name, r.cmd, r.line, BadReply}
	}

	if len(r.args) != 2 {
		return nil, Error{c.Name, r.cmd, r.line, BadReply}
	}

	id, err := strconv.Atoui64(r.args[0])

	if err != nil {
		return nil, Error{c.Name, r.cmd, r.line, BadReply}
	}

	return &Job{id, r.body, c}, nil
}

func (r result) checkForInt(c *Conn, s string) (uint64, os.Error) {
	if r.err != nil {
		return 0, Error{c.Name, r.cmd, r.line, r.err}
	}

	if err, ok := replyErrors[r.name]; ok {
		return 0, Error{c.Name, r.cmd, r.line, err}
	}

	if r.name != s {
		return 0, Error{c.Name, r.cmd, r.line, BadReply}
	}

	if len(r.args) != 1 {
		return 0, Error{c.Name, r.cmd, r.line, BadReply}
	}

	n, err := strconv.Atoui64(r.args[0])

	if err != nil {
		return 0, Error{c.Name, r.cmd, r.line, BadReply}
	}

	return n, nil
}

func (r result) checkForWord(c *Conn, s string) os.Error {
	if r.err != nil {
		return Error{c.Name, r.cmd, r.line, r.err}
	}

	if err, ok := replyErrors[r.name]; ok && r.name != s {
		return Error{c.Name, r.cmd, r.line, err}
	}

	if r.name != s {
		return Error{c.Name, r.cmd, r.line, BadReply}
	}

	return nil
}

func parseDict(s string) map[string]string {
	d := make(map[string]string)
	if strings.HasPrefix(s, "---") {
		s = s[3:]
	}
	s = strings.TrimSpace(s)
	lines := strings.SplitN(s, "\n", 0)
	for _, line := range lines {
		kv := strings.SplitN(line, ": ", 2)
		if len(kv) != 2 {
			continue
		}
		k, v := kv[0], kv[1]
		d[k] = v
	}
	return d
}

func parseList(s string) []string {
	if strings.HasPrefix(s, "---") {
		s = s[3:]
	}
	s = strings.TrimSpace(s)
	lines := strings.SplitN(s, "\n", 0)
	a := make([]string, 0, len(lines))
	for _, line := range lines {
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		a = a[0:len(a) + 1]
		a[len(a) - 1] = line[2:]
	}
	return a
}

func (r result) checkForDict(c *Conn) (map[string]string, os.Error) {
	if r.err != nil {
		return nil, Error{c.Name, r.cmd, r.line, r.err}
	}

	if r.name != "OK" {
		return nil, Error{c.Name, r.cmd, r.line, BadReply}
	}

	if len(r.args) != 1 {
		return nil, Error{c.Name, r.cmd, r.line, BadReply}
	}

	return parseDict(r.body), nil
}

func (r result) checkForList(c *Conn) ([]string, os.Error) {
	if r.err != nil {
		return nil, Error{c.Name, r.cmd, r.line, r.err}
	}

	if r.name != "OK" {
		return nil, Error{c.Name, r.cmd, r.line, BadReply}
	}

	if len(r.args) != 1 {
		return nil, Error{c.Name, r.cmd, r.line, BadReply}
	}

	return parseList(r.body), nil
}

// Get a copy of the specified job.
func (c *Conn) Peek(id uint64) (*Job, os.Error) {
	return c.cmd("peek %d\r\n", id).checkForJob(c, "FOUND")
}

func (c *Conn) Stats() (map[string]string, os.Error) {
	return c.cmd("stats\r\n").checkForDict(c)
}

func (c *Conn) ListTubes() ([]string, os.Error) {
	return c.cmd("list-tubes\r\n").checkForList(c)
}

func okTubeChars(name string) bool {
	return nameRegexp.MatchString(name) && len(name) < 201
}

// Returns an error if the tube name is invalid.
func NewTube(c *Conn, name string) (*Tube, os.Error) {
	if len(name) > 200 {
		return nil, TubeError{name, NameTooLong}
	}
	if !okTubeChars(name) {
		return nil, TubeError{name, IllegalChar}
	}
	return &Tube{Name: name, C: c}, nil
}

// Returns an error if any of the tube names are invalid.
func NewTubeSet(c *Conn, names []string) (*TubeSet, os.Error) {
	for _, name := range names {
		if len(name) > 200 {
			return nil, TubeError{name, NameTooLong}
		}
		if !okTubeChars(name) {
			return nil, TubeError{name, IllegalChar}
		}
	}
	return &TubeSet{names, Forever, c}, nil
}

// Reserve a job from any one of the tubes in t.
func (t TubeSet) Reserve() (*Job, os.Error) {
	for {
		r := t.cmd("reserve-with-timeout %d\r\n", seconds(t.UsTimeout))
		j, err := r.checkForJob(t.C, "RESERVED")
		e, ok := err.(Error)
		if ok && e.Error == deadlineSoon {
			// Retry automatically
			// TODO be careful not to flood
			continue
		}
		return j, err
	}
	panic("not reached")
}

// Get a copy of the next ready job in this tube, if any.
func (t Tube) PeekReady() (*Job, os.Error) {
	return t.cmd("peek-ready\r\n").checkForJob(t.C, "FOUND")
}

// Get a copy of the next delayed job in this tube, if any.
func (t Tube) PeekDelayed() (*Job, os.Error) {
	return t.cmd("peek-delayed\r\n").checkForJob(t.C, "FOUND")
}

// Get a copy of a buried job in this tube, if any.
func (t Tube) PeekBuried() (*Job, os.Error) {
	return t.cmd("peek-buried\r\n").checkForJob(t.C, "FOUND")
}

// Get statistics on tube t.
func (t Tube) Stats() (map[string]string, os.Error) {
	// Note: do not use t.cmd -- this doesn't depend on the "currently
	// used" tube.
	return t.C.cmd("stats-tube %s\r\n", t.Name).checkForDict(t.C)
}

// Kick up to n jobs in tube t.
func (t Tube) Kick(n uint64) (uint64, os.Error) {
	return t.cmd("kick %d\r\n", n).checkForInt(t.C, "KICKED")
}

// Pause tube t for µs microseconds.
func (t Tube) Pause(µs uint64) os.Error {
	// Note: do not use t.cmd -- this doesn't depend on the "currently
	// used" tube.
	r := t.C.cmd("pause-tube %s %d\r\n", t.Name, µs)
	return r.checkForWord(t.C, "PAUSED")
}

// Delete job j.
func (j Job) Delete() os.Error {
	return j.C.cmd("delete %d\r\n", j.Id).checkForWord(j.C, "DELETED")
}

// Touch job j.
func (j Job) Touch() os.Error {
	return j.C.cmd("touch %d\r\n", j.Id).checkForWord(j.C, "TOUCHED")
}

// Bury job j and change its priority to pri.
func (j Job) Bury(pri uint32) os.Error {
	return j.C.cmd("bury %d %d\r\n", j.Id, pri).checkForWord(j.C, "BURIED")
}

// Release job j, changing its priority to pri and its delay to delay.
func (j Job) Release(pri uint32, µsDelay uint64) os.Error {
	r := j.C.cmd("release %d %d %d\r\n", j.Id, pri, seconds(µsDelay))
	return r.checkForWord(j.C, "RELEASED")
}

// Get statistics on job j.
func (j Job) Stats() (map[string]string, os.Error) {
	return j.C.cmd("stats-job %d\r\n", j.Id).checkForDict(j.C)
}
