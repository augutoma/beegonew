// Copyright 2014 beego Author. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package logs provide a general log interface
// Usage:
//
// import "github.com/augutoma/beegonew/logs"
//
//	log := NewLogger(10000)
//	log.SetLogger("console", "")
//
//	> the first params stand for how many channel
//
// Use it like this:
//
//	log.Trace("trace")
//	log.Info("info")
//	log.Warn("warning")
//	log.Debug("debug")
//	log.Critical("critical")
//
//  more docs http://beego.me/docs/module/logs.md
package logs

import (
	"fmt"
	"log"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"context"
)

// RFC5424 log message levels.
const (
	LevelEmergency = iota
	LevelAlert
	LevelCritical
	LevelError
	LevelWarning
	LevelNotice
	LevelInformational
	LevelDebug
)

// levelLogLogger is defined to implement log.Logger
// the real log level will be LevelEmergency
const levelLoggerImpl = -1

// Name for adapter with beego official support
const (
	AdapterConsole   = "console"
	AdapterFile      = "file"
	AdapterMultiFile = "multifile"
	AdapterMail      = "smtp"
	AdapterConn      = "conn"
	AdapterEs        = "es"
	AdapterJianLiao  = "jianliao"
	AdapterSlack     = "slack"
	AdapterAliLS     = "alils"
)

// Legacy log level constants to ensure backwards compatibility.
const (
	LevelInfo  = LevelInformational
	LevelTrace = LevelDebug
	LevelWarn  = LevelWarning
)

type newLoggerFunc func() Logger

// Logger defines the behavior of a log provider.
type Logger interface {
	Init(config string) error
	WriteMsg(when time.Time, msg string, level int) error
	Destroy()
	Flush()
}

var adapters = make(map[string]newLoggerFunc)
var levelPrefix = [LevelDebug + 1]string{"[M] ", "[A] ", "[C] ", "[E] ", "[W] ", "[N] ", "[I] ", "[D] "}

// Register makes a log provide available by the provided name.
// If Register is called twice with the same name or if driver is nil,
// it panics.
func Register(name string, log newLoggerFunc) {
	if log == nil {
		panic("logs: Register provide is nil")
	}
	if _, dup := adapters[name]; dup {
		panic("logs: Register called twice for provider " + name)
	}
	adapters[name] = log
}

// BeeLogger is default logger in beego application.
// it can contain several providers and log message into all providers.
type BeeLogger struct {
	lock                sync.Mutex
	level               int
	init                bool
	enableFuncCallDepth bool
	loggerFuncCallDepth int
	asynchronous        bool
	msgChanLen          int64
	msgChan             chan *logMsg
	signalChan          chan string
	wg                  sync.WaitGroup
	outputs             []*nameLogger
}

const defaultAsyncMsgLen = 1e3

type nameLogger struct {
	Logger
	name string
}

type logMsg struct {
	level int
	msg   string
	when  time.Time
}

var logMsgPool *sync.Pool

// NewLogger returns a new BeeLogger.
// channelLen means the number of messages in chan(used where asynchronous is true).
// if the buffering chan is full, logger adapters write to file or other way.
func NewLogger(channelLens ...int64) *BeeLogger {
	bl := new(BeeLogger)
	bl.level = LevelDebug
	bl.loggerFuncCallDepth = 2
	bl.msgChanLen = append(channelLens, 0)[0]
	if bl.msgChanLen <= 0 {
		bl.msgChanLen = defaultAsyncMsgLen
	}
	bl.signalChan = make(chan string, 1)
	bl.setLogger(AdapterConsole)
	return bl
}

// Async set the log to asynchronous and start the goroutine
func (bl *BeeLogger) Async(msgLen ...int64) *BeeLogger {
	bl.lock.Lock()
	defer bl.lock.Unlock()
	if bl.asynchronous {
		return bl
	}
	bl.asynchronous = true
	if len(msgLen) > 0 && msgLen[0] > 0 {
		bl.msgChanLen = msgLen[0]
	}
	bl.msgChan = make(chan *logMsg, bl.msgChanLen)
	logMsgPool = &sync.Pool{
		New: func() interface{} {
			return &logMsg{}
		},
	}
	bl.wg.Add(1)
	go bl.startLogger()
	return bl
}

// SetLogger provides a given logger adapter into BeeLogger with config string.
// config need to be correct JSON as string: {"interval":360}.
func (bl *BeeLogger) setLogger(adapterName string, configs ...string) error {
	config := append(configs, "{}")[0]
	for _, l := range bl.outputs {
		if l.name == adapterName {
			return fmt.Errorf("logs: duplicate adaptername %q (you have set this logger before)", adapterName)
		}
	}

	log, ok := adapters[adapterName]
	if !ok {
		return fmt.Errorf("logs: unknown adaptername %q (forgotten Register?)", adapterName)
	}

	lg := log()
	err := lg.Init(config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logs.BeeLogger.SetLogger: "+err.Error())
		return err
	}
	bl.outputs = append(bl.outputs, &nameLogger{name: adapterName, Logger: lg})
	return nil
}

// SetLogger provides a given logger adapter into BeeLogger with config string.
// config need to be correct JSON as string: {"interval":360}.
func (bl *BeeLogger) SetLogger(adapterName string, configs ...string) error {
	bl.lock.Lock()
	defer bl.lock.Unlock()
	if !bl.init {
		bl.outputs = []*nameLogger{}
		bl.init = true
	}
	return bl.setLogger(adapterName, configs...)
}

// DelLogger remove a logger adapter in BeeLogger.
func (bl *BeeLogger) DelLogger(adapterName string) error {
	bl.lock.Lock()
	defer bl.lock.Unlock()
	outputs := []*nameLogger{}
	for _, lg := range bl.outputs {
		if lg.name == adapterName {
			lg.Destroy()
		} else {
			outputs = append(outputs, lg)
		}
	}
	if len(outputs) == len(bl.outputs) {
		return fmt.Errorf("logs: unknown adaptername %q (forgotten Register?)", adapterName)
	}
	bl.outputs = outputs
	return nil
}

func (bl *BeeLogger) writeToLoggers(when time.Time, msg string, level int) {
	for _, l := range bl.outputs {
		err := l.WriteMsg(when, msg, level)
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to WriteMsg to adapter:%v,error:%v\n", l.name, err)
		}
	}
}

func (bl *BeeLogger) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	// writeMsg will always add a '\n' character
	if p[len(p)-1] == '\n' {
		p = p[0 : len(p)-1]
	}
	// set levelLoggerImpl to ensure all log message will be write out
	err = bl.writeMsg(levelLoggerImpl, string(p))
	if err == nil {
		return len(p), err
	}
	return 0, err
}
func (bl *BeeLogger) writeMsg(logLevel int, msg string, v ...interface{}) error {
	if !bl.init {
		bl.lock.Lock()
		bl.setLogger(AdapterConsole)
		bl.lock.Unlock()
	}

	if len(v) > 0 {
		msg = fmt.Sprintf(msg, v...)
	}
	when := time.Now()
	if bl.enableFuncCallDepth {
		pc, file, line, ok := runtime.Caller(bl.loggerFuncCallDepth)
		if !ok {
			file = "???"
			line = 0
		}
		_, filename := path.Split(file)

		details := runtime.FuncForPC(pc)
		fname := ""
		if details != nil  {
			fname = details.Name()[strings.LastIndex(details.Name(), ".")+1:]
		}
		msg = fmt.Sprintf("%-43v", "[" + strings.TrimSuffix(filename, ".go") + "/" + fname + ":" + strconv.FormatInt(int64(line), 10) + "]") + msg

		msg = fmt.Sprintf("%10v","***") + "~ " + msg
		msg = fmt.Sprintf("%5v", "0") + ";" + msg
		msg = fmt.Sprintf("%15v","0.0.0.0") + "^" + msg
		msg = fmt.Sprintf("%6v", "0") + "&" + msg
	}

	//set level info in front of filename info
	if logLevel == levelLoggerImpl {
		// set to emergency to ensure all log will be print out correctly
		logLevel = LevelEmergency
		msg = levelPrefix[0] + msg
	} else {
		msg = levelPrefix[logLevel] + msg
	}

	if bl.asynchronous {
		lm := logMsgPool.Get().(*logMsg)
		lm.level = logLevel
		lm.msg = msg
		lm.when = when
		bl.msgChan <- lm
	} else {
		bl.writeToLoggers(when, msg, logLevel)
	}
	return nil
}

func (bl *BeeLogger) writeMsgdb(ctx context.Context, logLevel int, msg string, v ...interface{}) error {
	if !bl.init {
		bl.lock.Lock()
		bl.setLogger(AdapterConsole)
		bl.lock.Unlock()
	}

	if len(v) > 0 {
		msg = fmt.Sprintf(msg, v...)
	}
	when := time.Now()
	if bl.enableFuncCallDepth {
		pc, file, line, ok := runtime.Caller(bl.loggerFuncCallDepth+2)
		if !ok {
			file = "???"
			line = 0
		}
		_, filename := path.Split(file)

		details := runtime.FuncForPC(pc)
		fname := ""
		if details != nil  {
			fname = details.Name()[strings.LastIndex(details.Name(), ".")+1:]
		}
		msg = fmt.Sprintf("%-43s", "[" + strings.TrimSuffix(filename, ".go") + "/" + fname + ":" + strconv.FormatInt(int64(line), 10) + "]") + msg

		if ctxRqId, ok := ctx.Value("UserName").(string); ok {
			msg = fmt.Sprintf("%10s",ctxRqId) + "~ " + msg
		} else {
			//msg = fmt.Sprintf("%10s","***") + "~ " + msg
			msg = "       ***~ " + msg
		}

		if ctxRqId, ok := ctx.Value("RefID").(string); ok {
			msg = fmt.Sprintf("%5s",ctxRqId) + ";" + msg
		} else {
			msg = "    0;" + msg
		}

		if ctxRqIp, ok := ctx.Value("RefIP").(string); ok {
			msg = fmt.Sprintf("%15s",ctxRqIp) + "^" + msg
		} else {
			//msg = fmt.Sprintf("%15s","0.0.0.0") + "^" + msg
			msg = "        0.0.0.0^" + msg
		}

		if RefNS, ok := ctx.Value("RefNS").(int64); ok {
			if (RefNS != 0) {
				msg = fmt.Sprintf("%6d", (time.Now().UnixNano() - RefNS) / 1000000 ) + "&" + msg
			} else {
				msg = "     0&" + msg
			}
		} else {
			msg = "     0&" + msg
		}

	}

	//set level info in front of filename info
	if logLevel == levelLoggerImpl {
		// set to emergency to ensure all log will be print out correctly
		logLevel = LevelEmergency
	} else {
		msg = levelPrefix[logLevel] + msg
	}

	if bl.asynchronous {
		lm := logMsgPool.Get().(*logMsg)
		lm.level = logLevel
		lm.msg = msg
		lm.when = when
		bl.msgChan <- lm
	} else {
		bl.writeToLoggers(when, msg, logLevel)
	}
	return nil
}

func (bl *BeeLogger) writeMsgref(data map[interface{}]interface{}, logLevel int, msg string, v ...interface{}) error {
	if !bl.init {
		bl.lock.Lock()
		bl.setLogger(AdapterConsole)
		bl.lock.Unlock()
	}

	if len(v) > 0 {
		msg = fmt.Sprintf(msg, v...)
	}
	when := time.Now()
	if bl.enableFuncCallDepth {
		pc, file, line, ok := runtime.Caller(bl.loggerFuncCallDepth)
		if !ok {
			file = "???"
			line = 0
		}
		_, filename := path.Split(file)

		details := runtime.FuncForPC(pc)
		fname := ""
		if details != nil  {
			fname = details.Name()[strings.LastIndex(details.Name(), ".")+1:]
		}
		msg = "[" + filename + "/" + fname + ":" + strconv.FormatInt(int64(line), 10) + "]\t\t" + msg

		if data != nil {
			if val, ok := data["UserName"]; ok {
				msg = val.(string) + " ~~ " + msg
			} else {
				msg = "*** ~~ " + msg
			}

			if val, ok := data["RefID"]; ok {
				msg = val.(string) + " ;; " + msg
			} else {
				msg = "0 ;; " + msg
			}

			//if val, ok := data["RefID"]; ok {
			//	msg = strconv.FormatUint(uint64(val.(uint64)), 10) + " ;; " + msg
			//} else {
			//	msg = "0 ;; " + msg
			//}
		}
	}

	//set level info in front of filename info
	if logLevel == levelLoggerImpl {
		// set to emergency to ensure all log will be print out correctly
		logLevel = LevelEmergency
	} else {
		msg = levelPrefix[logLevel] + msg
	}

	if bl.asynchronous {
		lm := logMsgPool.Get().(*logMsg)
		lm.level = logLevel
		lm.msg = msg
		lm.when = when
		bl.msgChan <- lm
	} else {
		bl.writeToLoggers(when, msg, logLevel)
	}
	return nil
}

func (bl *BeeLogger) writeMsgctx(ctx context.Context, logLevel int, msg string, v ...interface{}) error {
	if !bl.init {
		bl.lock.Lock()
		bl.setLogger(AdapterConsole)
		bl.lock.Unlock()
	}

	if len(v) > 0 {
		msg = fmt.Sprintf(msg, v...)
	}
	when := time.Now()
	if bl.enableFuncCallDepth {
		pc, file, line, ok := runtime.Caller(bl.loggerFuncCallDepth)
		if !ok {
			file = "???"
			line = 0
		}
		_, filename := path.Split(file)

		details := runtime.FuncForPC(pc)
		fname := ""
		if details != nil  {
			fname = details.Name()[strings.LastIndex(details.Name(), ".")+1:]
		}
		msg = fmt.Sprintf("%-43s", "[" + strings.TrimSuffix(filename, ".go") + "/" + fname + ":" + strconv.FormatInt(int64(line), 10) + "]") + msg

		if ctxRqId, ok := ctx.Value("UserName").(string); ok {
			msg = fmt.Sprintf("%10s",ctxRqId) + "~ " + msg
		} else {
			//msg = fmt.Sprintf("%10s","***") + "~ " + msg
			msg = "       ***~ " + msg
		}

		if ctxRqId, ok := ctx.Value("RefID").(string); ok {
			msg = fmt.Sprintf("%5s",ctxRqId) + ";" + msg
		} else {
			//msg = fmt.Sprintf("%5s", "0") + ";" + msg
			msg = "    0;" + msg
		}

		if ctxRqIp, ok := ctx.Value("RefIP").(string); ok {
			msg = fmt.Sprintf("%15s",ctxRqIp) + "^" + msg
		} else {
			//msg = fmt.Sprintf("%15s","0.0.0.0") + "^" + msg
			msg = "        0.0.0.0^" + msg
		}

		if RefNS, ok := ctx.Value("RefNS").(int64); ok {
			if (RefNS != 0) {
				msg = fmt.Sprintf("%6d", (time.Now().UnixNano() - RefNS) / 1000000 ) + "&" + msg
			} else {
				msg = "     0&" + msg
			}
		} else {
			msg = "     0&" + msg
		}

	}

	//set level info in front of filename info
	if logLevel == levelLoggerImpl {
		// set to emergency to ensure all log will be print out correctly
		logLevel = LevelEmergency
	} else {
		msg = levelPrefix[logLevel] + msg
	}

	if bl.asynchronous {
		lm := logMsgPool.Get().(*logMsg)
		lm.level = logLevel
		lm.msg = msg
		lm.when = when
		bl.msgChan <- lm
	} else {
		bl.writeToLoggers(when, msg, logLevel)
	}
	return nil
}

// SetLevel Set log message level.
// If message level (such as LevelDebug) is higher than logger level (such as LevelWarning),
// log providers will not even be sent the message.
func (bl *BeeLogger) SetLevel(l int) {
	bl.level = l
}

// SetLogFuncCallDepth set log funcCallDepth
func (bl *BeeLogger) SetLogFuncCallDepth(d int) {
	bl.loggerFuncCallDepth = d
}

// GetLogFuncCallDepth return log funcCallDepth for wrapper
func (bl *BeeLogger) GetLogFuncCallDepth() int {
	return bl.loggerFuncCallDepth
}

// EnableFuncCallDepth enable log funcCallDepth
func (bl *BeeLogger) EnableFuncCallDepth(b bool) {
	bl.enableFuncCallDepth = b
}

// start logger chan reading.
// when chan is not empty, write logs.
func (bl *BeeLogger) startLogger() {
	gameOver := false
	for {
		select {
		case bm := <-bl.msgChan:
			bl.writeToLoggers(bm.when, bm.msg, bm.level)
			logMsgPool.Put(bm)
		case sg := <-bl.signalChan:
			// Now should only send "flush" or "close" to bl.signalChan
			bl.flush()
			if sg == "close" {
				for _, l := range bl.outputs {
					l.Destroy()
				}
				bl.outputs = nil
				gameOver = true
			}
			bl.wg.Done()
		}
		if gameOver {
			break
		}
	}
}

// Emergency Log EMERGENCY level message.
func (bl *BeeLogger) Emergency(format string, v ...interface{}) {
	if LevelEmergency > bl.level {
		return
	}
	bl.writeMsg(LevelEmergency, format, v...)
}

// Alert Log ALERT level message.
func (bl *BeeLogger) Alert(format string, v ...interface{}) {
	if LevelAlert > bl.level {
		return
	}
	bl.writeMsg(LevelAlert, format, v...)
}

// Critical Log CRITICAL level message.
func (bl *BeeLogger) Critical(format string, v ...interface{}) {
	if LevelCritical > bl.level {
		return
	}
	bl.writeMsg(LevelCritical, format, v...)
}

// Error Log ERROR level message.
func (bl *BeeLogger) Error(format string, v ...interface{}) {
	if LevelError > bl.level {
		return
	}
	bl.writeMsg(LevelError, format, v...)
}

// Warning Log WARNING level message.
func (bl *BeeLogger) Warning(format string, v ...interface{}) {
	if LevelWarn > bl.level {
		return
	}
	bl.writeMsg(LevelWarn, format, v...)
}

// Notice Log NOTICE level message.
func (bl *BeeLogger) Notice(format string, v ...interface{}) {
	if LevelNotice > bl.level {
		return
	}
	bl.writeMsg(LevelNotice, format, v...)
}

// Informational Log INFORMATIONAL level message.
func (bl *BeeLogger) Informational(format string, v ...interface{}) {
	if LevelInfo > bl.level {
		return
	}
	bl.writeMsg(LevelInfo, format, v...)
}

// Debug Log DEBUG level message.
func (bl *BeeLogger) Debug(format string, v ...interface{}) {
	if LevelDebug > bl.level {
		return
	}
	bl.writeMsg(LevelDebug, format, v...)
}

// Debugdb Log DEBUG level message.
func (bl *BeeLogger) Debugdb(ctx context.Context, format string, v ...interface{}) {
	if LevelDebug > bl.level {
		return
	}
	bl.writeMsgdb(ctx, LevelDebug, format, v...)
}

// Debugdb Log DEBUG level message.
func (bl *BeeLogger) Debugref(data map[interface{}]interface{}, format string, v ...interface{}) {
	if LevelDebug > bl.level {
		return
	}
	bl.writeMsgref(data, LevelDebug, format, v...)
}

// Debugdb Log DEBUG level message.
func (bl *BeeLogger) Noticeref(data map[interface{}]interface{}, format string, v ...interface{}) {
	if LevelDebug > bl.level {
		return
	}
	bl.writeMsgref(data, LevelNotice, format, v...)
}

// Debugdb Log DEBUG level message.
func (bl *BeeLogger) Traceref(data map[interface{}]interface{}, format string, v ...interface{}) {
	if LevelDebug > bl.level {
		return
	}
	bl.writeMsgref(data, LevelTrace, format, v...)
}

// Debugdb Log DEBUG level message.
func (bl *BeeLogger) Inforef(data map[interface{}]interface{}, format string, v ...interface{}) {
	if LevelDebug > bl.level {
		return
	}
	bl.writeMsgref(data, LevelInfo, format, v...)
}

// Debugdb Log DEBUG level message.
func (bl *BeeLogger) Warnref(data map[interface{}]interface{}, format string, v ...interface{}) {
	if LevelDebug > bl.level {
		return
	}
	bl.writeMsgref(data, LevelWarn, format, v...)
}

// Debugdb Log DEBUG level message.
func (bl *BeeLogger) Errorref(data map[interface{}]interface{}, format string, v ...interface{}) {
	if LevelDebug > bl.level {
		return
	}
	bl.writeMsgref(data, LevelError, format, v...)
}

// Debugctx Log DEBUG level message.
func (bl *BeeLogger) Debugctx(ctx context.Context, format string, v ...interface{}) {
	if LevelDebug > bl.level {
		return
	}
	bl.writeMsgctx(ctx, LevelDebug, format, v...)
}

// Noticectx Log NOTICE level message.
func (bl *BeeLogger) Noticectx(ctx context.Context, format string, v ...interface{}) {
	if LevelNotice > bl.level {
		return
	}
	bl.writeMsgctx(ctx, LevelNotice, format, v...)
}

// Tracectx Log TRACE level message.
func (bl *BeeLogger) Tracectx(ctx context.Context, format string, v ...interface{}) {
	if LevelTrace > bl.level {
		return
	}
	bl.writeMsgctx(ctx, LevelTrace, format, v...)
}

// Infoctx Log INFO level message.
func (bl *BeeLogger) Infoctx(ctx context.Context, format string, v ...interface{}) {
	if LevelInfo > bl.level {
		return
	}
	bl.writeMsgctx(ctx, LevelInfo, format, v...)
}

// Warnctx Log WARN level message.
func (bl *BeeLogger) Warnctx(ctx context.Context, format string, v ...interface{}) {
	if LevelWarn > bl.level {
		return
	}
	bl.writeMsgctx(ctx, LevelWarn, format, v...)
}

// Errorctx Log ERROR level message.
func (bl *BeeLogger) Errorctx(ctx context.Context, format string, v ...interface{}) {
	if LevelError > bl.level {
		return
	}
	bl.writeMsgctx(ctx, LevelError, format, v...)
}



// Warn Log WARN level message.
// compatibility alias for Warning()
func (bl *BeeLogger) Warn(format string, v ...interface{}) {
	if LevelWarn > bl.level {
		return
	}
	bl.writeMsg(LevelWarn, format, v...)
}

// Info Log INFO level message.
// compatibility alias for Informational()
func (bl *BeeLogger) Info(format string, v ...interface{}) {
	if LevelInfo > bl.level {
		return
	}
	bl.writeMsg(LevelInfo, format, v...)
}

// Trace Log TRACE level message.
// compatibility alias for Debug()
func (bl *BeeLogger) Trace(format string, v ...interface{}) {
	if LevelDebug > bl.level {
		return
	}
	bl.writeMsg(LevelDebug, format, v...)
}

// Flush flush all chan data.
func (bl *BeeLogger) Flush() {
	if bl.asynchronous {
		bl.signalChan <- "flush"
		bl.wg.Wait()
		bl.wg.Add(1)
		return
	}
	bl.flush()
}

// Close close logger, flush all chan data and destroy all adapters in BeeLogger.
func (bl *BeeLogger) Close() {
	if bl.asynchronous {
		bl.signalChan <- "close"
		bl.wg.Wait()
		close(bl.msgChan)
	} else {
		bl.flush()
		for _, l := range bl.outputs {
			l.Destroy()
		}
		bl.outputs = nil
	}
	close(bl.signalChan)
}

// Reset close all outputs, and set bl.outputs to nil
func (bl *BeeLogger) Reset() {
	bl.Flush()
	for _, l := range bl.outputs {
		l.Destroy()
	}
	bl.outputs = nil
}

func (bl *BeeLogger) flush() {
	if bl.asynchronous {
		for {
			if len(bl.msgChan) > 0 {
				bm := <-bl.msgChan
				bl.writeToLoggers(bm.when, bm.msg, bm.level)
				logMsgPool.Put(bm)
				continue
			}
			break
		}
	}
	for _, l := range bl.outputs {
		l.Flush()
	}
}

// beeLogger references the used application logger.
var beeLogger *BeeLogger = NewLogger()

// GetLogger returns the default BeeLogger
func GetBeeLogger() *BeeLogger {
	return beeLogger
}

var beeLoggerMap = struct {
	sync.RWMutex
	logs map[string]*log.Logger
}{
	logs: map[string]*log.Logger{},
}

// GetLogger returns the default BeeLogger
func GetLogger(prefixes ...string) *log.Logger {
	prefix := append(prefixes, "")[0]
	if prefix != "" {
		prefix = fmt.Sprintf(`[%s] `, strings.ToUpper(prefix))
	}
	beeLoggerMap.RLock()
	l, ok := beeLoggerMap.logs[prefix]
	if ok {
		beeLoggerMap.RUnlock()
		return l
	}
	beeLoggerMap.RUnlock()
	beeLoggerMap.Lock()
	defer beeLoggerMap.Unlock()
	l, ok = beeLoggerMap.logs[prefix]
	if !ok {
		l = log.New(beeLogger, prefix, 0)
		beeLoggerMap.logs[prefix] = l
	}
	return l
}

// Reset will remove all the adapter
func Reset() {
	beeLogger.Reset()
}

func Async(msgLen ...int64) *BeeLogger {
	return beeLogger.Async(msgLen...)
}

// SetLevel sets the global log level used by the simple logger.
func SetLevel(l int) {
	beeLogger.SetLevel(l)
}

// EnableFuncCallDepth enable log funcCallDepth
func EnableFuncCallDepth(b bool) {
	beeLogger.enableFuncCallDepth = b
}

// SetLogFuncCall set the CallDepth, default is 4
func SetLogFuncCall(b bool) {
	beeLogger.EnableFuncCallDepth(b)
	beeLogger.SetLogFuncCallDepth(4)
}

// SetLogFuncCallDepth set log funcCallDepth
func SetLogFuncCallDepth(d int) {
	beeLogger.loggerFuncCallDepth = d
}

// SetLogger sets a new logger.
func SetLogger(adapter string, config ...string) error {
	err := beeLogger.SetLogger(adapter, config...)
	if err != nil {
		return err
	}
	return nil
}

// Emergency logs a message at emergency level.
func Emergency(f interface{}, v ...interface{}) {
	beeLogger.Emergency(formatLog(f, v...))
}

// Alert logs a message at alert level.
func Alert(f interface{}, v ...interface{}) {
	beeLogger.Alert(formatLog(f, v...))
}

// Critical logs a message at critical level.
func Critical(f interface{}, v ...interface{}) {
	beeLogger.Critical(formatLog(f, v...))
}

// Error logs a message at error level.
func Error(f interface{}, v ...interface{}) {
	beeLogger.Error(formatLog(f, v...))
}

// Warning logs a message at warning level.
func Warning(f interface{}, v ...interface{}) {
	beeLogger.Warn(formatLog(f, v...))
}

// Warn compatibility alias for Warning()
func Warn(f interface{}, v ...interface{}) {
	beeLogger.Warn(formatLog(f, v...))
}

// Notice logs a message at notice level.
func Notice(f interface{}, v ...interface{}) {
	beeLogger.Notice(formatLog(f, v...))
}

// Informational logs a message at info level.
func Informational(f interface{}, v ...interface{}) {
	beeLogger.Info(formatLog(f, v...))
}

// Info compatibility alias for Warning()
func Info(f interface{}, v ...interface{}) {
	beeLogger.Info(formatLog(f, v...))
}

// Debug logs a message at debug level.
func Debug(f interface{}, v ...interface{}) {
	beeLogger.Debug(formatLog(f, v...))
}

// Debug logs a message at debug level.
func Debugdb(ctx context.Context, f interface{}, v ...interface{}) {
	beeLogger.Debugdb(ctx, formatLog(f, v...))
}

// Debug logs a message at debug level.
func Debugref(data map[interface{}]interface{}, f interface{}, v ...interface{}) {
	beeLogger.Debugref(data, formatLog(f, v...))
}

// Debug logs a message at debug level.
func Noticeref(data map[interface{}]interface{}, f interface{}, v ...interface{}) {
	beeLogger.Noticeref(data, formatLog(f, v...))
}

// Debug logs a message at debug level.
func Traceref(data map[interface{}]interface{}, f interface{}, v ...interface{}) {
	beeLogger.Traceref(data, formatLog(f, v...))
}

// Debug logs a message at debug level.
func Inforef(data map[interface{}]interface{}, f interface{}, v ...interface{}) {
	beeLogger.Inforef(data, formatLog(f, v...))
}

// Debug logs a message at debug level.
func Warnref(data map[interface{}]interface{}, f interface{}, v ...interface{}) {
	beeLogger.Warnref(data, formatLog(f, v...))
}

// Debug logs a message at debug level.
func Errorref(data map[interface{}]interface{}, f interface{}, v ...interface{}) {
	beeLogger.Errorref(data, formatLog(f, v...))
}

// Debugctx logs a message at debug level.
func Debugctx(ctx context.Context, f interface{}, v ...interface{}) {
	beeLogger.Debugctx(ctx, formatLog(f, v...))
}

// Noticectx logs a message at debug level.
func Noticectx(ctx context.Context, f interface{}, v ...interface{}) {
	beeLogger.Noticectx(ctx, formatLog(f, v...))
}

// Tracectx logs a message at debug level.
func Tracectx(ctx context.Context, f interface{}, v ...interface{}) {
	beeLogger.Tracectx(ctx, formatLog(f, v...))
}

// Infoctx logs a message at debug level.
func Infoctx(ctx context.Context, f interface{}, v ...interface{}) {
	beeLogger.Infoctx(ctx, formatLog(f, v...))
}

// Warnctx logs a message at debug level.
func Warnctx(ctx context.Context, f interface{}, v ...interface{}) {
	beeLogger.Warnctx(ctx, formatLog(f, v...))
}

// Errorctx logs a message at debug level.
func Errorctx(ctx context.Context, f interface{}, v ...interface{}) {
	beeLogger.Errorctx(ctx, formatLog(f, v...))
}





// Trace logs a message at trace level.
// compatibility alias for Warning()
func Trace(f interface{}, v ...interface{}) {
	beeLogger.Trace(formatLog(f, v...))
}

func formatLog(f interface{}, v ...interface{}) string {
	var msg string
	switch f.(type) {
	case string:
		msg = f.(string)
		if len(v) == 0 {
			return msg
		}
		if strings.Contains(msg, "%") && !strings.Contains(msg, "%%") {
			//format string
		} else {
			//do not contain format char
			msg += strings.Repeat(" %v", len(v))
		}
	default:
		msg = fmt.Sprint(f)
		if len(v) == 0 {
			return msg
		}
		msg += strings.Repeat(" %v", len(v))
	}
	return fmt.Sprintf(msg, v...)
}
