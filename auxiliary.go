package lua

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

func functionName(l *State, d Debug) string {
	switch {
	case d.NameKind != "":
		return fmt.Sprintf("function '%s'", d.Name)
	case d.What == "main":
		return "main chunk"
	case d.What == "Go":
		if pushGlobalFunctionName(l, d.callInfo) {
			s, _ := l.ToString(-1)
			l.Pop(1)
			return fmt.Sprintf("function '%s'", s)
		}
		return "?"
	}
	return fmt.Sprintf("function <%s:%d>", d.ShortSource, d.LineDefined)
}

func countLevels(l *State) int {
	li, le := 1, 1
	for _, ok := Stack(l, le); ok; _, ok = Stack(l, le) {
		li = le
		le *= 2
	}
	for li < le {
		m := (li + le) / 2
		if _, ok := Stack(l, m); ok {
			li = m + 1
		} else {
			le = m
		}
	}
	return le - 1
}

// Traceback creates and pushes a traceback of the stack l1. If message is not
// nil it is appended at the beginning of the traceback. The level parameter
// tells at which level to start the traceback.
func Traceback(l, l1 *State, message string, level int) {
	const levels1, levels2 = 12, 10
	levels := countLevels(l1)
	mark := 0
	if levels > levels1+levels2 {
		mark = levels1
	}
	buf := message
	if buf != "" {
		buf += "\n"
	}
	buf += "stack traceback:"
	for f, ok := Stack(l1, level); ok; f, ok = Stack(l1, level) {
		if level++; level == mark {
			buf += "\n\t..."
			level = levels - levels2
		} else {
			d, _ := Info(l1, "Slnt", f)
			buf += "\n\t" + d.ShortSource + ":"
			if d.CurrentLine > 0 {
				buf += fmt.Sprintf("%d:", d.CurrentLine)
			}
			buf += " in " + functionName(l, d)
			if d.IsTailCall {
				buf += "\n\t(...tail calls...)"
			}
		}
	}
	l.PushString(buf)
}

// MetaField pushes onto the stack the field event from the metatable of the
// object at index. If the object does not have a metatable, or if the
// metatable does not have this field, returns false and pushes nothing.
func MetaField(l *State, index int, event string) bool {
	if !l.MetaTable(index) {
		return false
	}
	l.PushString(event)
	l.RawGet(-2)
	if l.IsNil(-1) {
		l.Pop(2) // remove metatable and metafield
		return false
	}
	l.Remove(-2) // remove only metatable
	return true
}

// CallMeta calls a metamethod.
//
// If the object at index has a metatable and this metatable has a field event,
// this function calls this field passing the object as its only argument. In
// this case this function returns true and pushes onto the stack the value
// returned by the call. If there is no metatable or no metamethod, this
// function returns false (without pushing any value on the stack).
func CallMeta(l *State, index int, event string) bool {
	index = l.AbsIndex(index)
	if !MetaField(l, index, event) {
		return false
	}
	l.PushValue(index)
	l.Call(1, 1)
	return true
}

// ArgumentError raises an error with a standard message that includes extraMessage as a comment.
//
// This function never returns. It is an idiom to use it in Go functions as
//  lua.ArgumentError(l, args, "message")
//  panic("unreachable")
func ArgumentError(l *State, argCount int, extraMessage string) {
	f, ok := Stack(l, 0)
	if !ok { // no stack frame?
		Errorf(l, "bad argument #%d (%s)", argCount, extraMessage)
		return
	}
	d, _ := Info(l, "n", f)
	if d.NameKind == "method" {
		argCount--         // do not count 'self'
		if argCount == 0 { // error is in the self argument itself?
			Errorf(l, "calling '%s' on bad self (%s)", d.Name, extraMessage)
			return
		}
	}
	if d.Name == "" {
		if pushGlobalFunctionName(l, f) {
			d.Name, _ = l.ToString(-1)
		} else {
			d.Name = "?"
		}
	}
	Errorf(l, "bad argument #%d to '%s' (%s)", argCount, d.Name, extraMessage)
}

func findField(l *State, objectIndex, level int) bool {
	if level == 0 || !l.IsTable(-1) {
		return false
	}
	for l.PushNil(); l.Next(-2); l.Pop(1) { // for each pair in table
		if l.IsString(-2) { // ignore non-string keys
			if l.RawEqual(objectIndex, -1) { // found object?
				l.Pop(1) // remove value (but keep name)
				return true
			} else if findField(l, objectIndex, level-1) { // try recursively
				l.Remove(-2) // remove table (but keep name)
				l.PushString(".")
				l.Insert(-2) // place "." between the two names
				l.Concat(3)
				return true
			}
		}
	}
	return false
}

func pushGlobalFunctionName(l *State, f Frame) bool {
	top := l.Top()
	Info(l, "f", f) // push function
	l.PushGlobalTable()
	if findField(l, top+1, 2) {
		l.Copy(-1, top+1) // move name to proper place
		l.Pop(2)          // remove pushed values
		return true
	}
	l.SetTop(top) // remove function and global table
	return false
}

func typeError(l *State, argCount int, typeName string) {
	ArgumentError(l, argCount, l.PushString(typeName+" expected, got "+TypeNameOf(l, argCount)))
}

func tagError(l *State, argCount int, tag Type) { typeError(l, argCount, tag.String()) }

// Where pushes onto the stack a string identifying the current position of
// the control at level in the call stack. Typically this string has the
// following format:
//   chunkname:currentline:
// Level 0 is the running function, level 1 is the function that called the
// running function, etc.
//
// This function is used to build a prefix for error messages.
func Where(l *State, level int) {
	if f, ok := Stack(l, level); ok { // check function at level
		ar, _ := Info(l, "Sl", f) // get info about it
		if ar.CurrentLine > 0 {   // is there info?
			l.PushString(fmt.Sprintf("%s:%d: ", ar.ShortSource, ar.CurrentLine))
			return
		}
	}
	l.PushString("") // else, no information available...
}

// Errorf raises an error. The error message format is given by format plus
// any extra arguments, following the same rules as PushFString. It also adds
// at the beginning of the message the file name and the line number where
// the error occurred, if this information is available.
//
// This function never returns. It is an idiom to use it in Go functions as:
//   lua.Errorf(l, args)
//   panic("unreachable")
func Errorf(l *State, format string, a ...interface{}) {
	Where(l, 1)
	l.PushFString(format, a...)
	l.Concat(2)
	l.Error()
}

// ToStringMeta converts any Lua value at the given index to a Go string in a
// reasonable format. The resulting string is pushed onto the stack and also
// returned by the function.
//
// If the value has a metatable with a "__tostring" field, then ToStringMeta
// calls the corresponding metamethod with the value as argument, and uses
// the result of the call as its result.
func ToStringMeta(l *State, index int) (string, bool) {
	if !CallMeta(l, index, "__tostring") {
		switch l.TypeOf(index) {
		case TypeNumber, TypeString:
			l.PushValue(index)
		case TypeBoolean:
			if l.ToBoolean(index) {
				l.PushString("true")
			} else {
				l.PushString("false")
			}
		case TypeNil:
			l.PushString("nil")
		default:
			l.PushFString("%s: %p", TypeNameOf(l, index), l.ToValue(index))
		}
	}
	return l.ToString(-1)
}

// NewMetaTable returns false if the registry already has the key name. Otherwise,
// creates a new table to be used as a metatable for userdata, adds it to the
// registry with key name, and returns true.
//
// In both cases it pushes onto the stack the final value associated with name in
// the registry.
func NewMetaTable(l *State, name string) bool {
	if MetaTableNamed(l, name); !l.IsNil(-1) {
		return false
	}
	l.Pop(1)
	l.NewTable()
	l.PushValue(-1)
	l.SetField(RegistryIndex, name)
	return true
}

func MetaTableNamed(l *State, name string) {
	l.Field(RegistryIndex, name)
}

func SetMetaTableNamed(l *State, name string) {
	MetaTableNamed(l, name)
	l.SetMetaTable(-2)
}

func TestUserData(l *State, index int, name string) interface{} {
	if d := l.ToUserData(index); d != nil {
		if l.MetaTable(index) {
			if MetaTableNamed(l, name); !l.RawEqual(-1, -2) {
				d = nil
			}
			l.Pop(2)
			return d
		}
	}
	return nil
}

// CheckUserData checks whether the function argument at index is a userdata
// of the type name (see NewMetaTable) and returns the userdata (see
// ToUserData).
func CheckUserData(l *State, index int, name string) interface{} {
	if d := TestUserData(l, index, name); d != nil {
		return d
	}
	typeError(l, index, name)
	panic("unreachable")
}

// CheckType checks whether the function argument at index has type t. See Type for the encoding of types for t.
func CheckType(l *State, index int, t Type) {
	if l.TypeOf(index) != t {
		tagError(l, index, t)
	}
}

// CheckAny checks whether the function has an argument of any type (including nil) at position index.
func CheckAny(l *State, index int) {
	if l.TypeOf(index) == TypeNone {
		ArgumentError(l, index, "value expected")
	}
}

// ArgumentCheck checks whether cond is true. If not, raises an error with a standard message.
func ArgumentCheck(l *State, cond bool, index int, extraMessage string) {
	if !cond {
		ArgumentError(l, index, extraMessage)
	}
}

// CheckString checks whether the function argument at index is a string and returns this string.
//
// This function uses ToString to get its result, so all conversions and caveats of that function apply here.
func CheckString(l *State, index int) string {
	if s, ok := l.ToString(index); ok {
		return s
	}
	tagError(l, index, TypeString)
	panic("unreachable")
}

// OptString returns the string at index if it is a string. If this argument is
// absent or is nil, returns def. Otherwise, raises an error.
func OptString(l *State, index int, def string) string {
	if l.IsNoneOrNil(index) {
		return def
	}
	return CheckString(l, index)
}

func CheckNumber(l *State, index int) float64 {
	n, ok := l.ToNumber(index)
	if !ok {
		tagError(l, index, TypeNumber)
	}
	return n
}

func OptNumber(l *State, index int, def float64) float64 {
	if l.IsNoneOrNil(index) {
		return def
	}
	return CheckNumber(l, index)
}

func CheckInteger(l *State, index int) int {
	i, ok := l.ToInteger(index)
	if !ok {
		tagError(l, index, TypeNumber)
	}
	return i
}

func OptInteger(l *State, index, def int) int {
	if l.IsNoneOrNil(index) {
		return def
	}
	return CheckInteger(l, index)
}

func CheckUnsigned(l *State, index int) uint {
	i, ok := l.ToUnsigned(index)
	if !ok {
		tagError(l, index, TypeNumber)
	}
	return i
}

func OptUnsigned(l *State, index int, def uint) uint {
	if l.IsNoneOrNil(index) {
		return def
	}
	return CheckUnsigned(l, index)
}

func TypeNameOf(l *State, index int) string { return l.TypeOf(index).String() }

func SetFunctions(l *State, functions []RegistryFunction, upValueCount uint8) {
	uvCount := int(upValueCount)
	CheckStackWithMessage(l, uvCount, "too many upvalues")
	for _, r := range functions { // fill the table with given functions
		for i := 0; i < uvCount; i++ { // copy upvalues to the top
			l.PushValue(-uvCount)
		}
		l.PushGoClosure(r.Function, upValueCount) // closure with those upvalues
		l.SetField(-(uvCount + 2), r.Name)
	}
	l.Pop(uvCount) // remove upvalues
}

func CheckStackWithMessage(l *State, space int, message string) {
	// keep some extra space to run error routines, if needed
	if !l.CheckStack(space + MinStack) {
		if message != "" {
			Errorf(l, "stack overflow (%s)", message)
		} else {
			Errorf(l, "stack overflow")
		}
	}
}

func CheckOption(l *State, index int, def string, list []string) int {
	var name string
	if def == "" {
		name = OptString(l, index, def)
	} else {
		name = CheckString(l, index)
	}
	for i, s := range list {
		if name == s {
			return i
		}
	}
	ArgumentError(l, index, l.PushFString("invalid option '%s'", name))
	panic("unreachable")
}

func SubTable(l *State, index int, name string) bool {
	l.Field(index, name)
	if l.IsTable(-1) {
		return true // table already there
	}
	l.Pop(1) // remove previous result
	index = l.AbsIndex(index)
	l.NewTable()
	l.PushValue(-1)         // copy to be left at top
	l.SetField(index, name) // assign new table to field
	return false            // did not find table there
}

// Require calls function f with string name as an argument and sets the call
// result in package.loaded[name], as if that function had been called
// through require.
//
// If global is true, also stores the result into global name.
//
// Leaves a copy of that result on the stack.
func Require(l *State, name string, f Function, global bool) {
	l.PushGoFunction(f)
	l.PushString(name) // argument to f
	l.Call(1, 1)       // open module
	SubTable(l, RegistryIndex, "_LOADED")
	l.PushValue(-2)      // make copy of module (call result)
	l.SetField(-2, name) // _LOADED[name] = module
	l.Pop(1)             // remove _LOADED table
	if global {
		l.PushValue(-1)   // copy of module
		l.SetGlobal(name) // _G[name] = module
	}
}

func NewLibraryTable(l *State, functions []RegistryFunction) { l.CreateTable(0, len(functions)) }

func NewLibrary(l *State, functions []RegistryFunction) {
	NewLibraryTable(l, functions)
	SetFunctions(l, functions, 0)
}

func skipComment(r *bufio.Reader) (bool, error) {
	bom := "\xEF\xBB\xBF"
	if ba, err := r.Peek(len(bom)); err != nil && err != io.EOF {
		return false, err
	} else if string(ba) == bom {
		_, _ = r.Read(ba)
	}
	if c, _, err := r.ReadRune(); err != nil {
		if err == io.EOF {
			err = nil
		}
		return false, err
	} else if c == '#' {
		_, err = r.ReadBytes('\n')
		if err == io.EOF {
			err = nil
		}
		return true, err
	}
	return false, r.UnreadRune()
}

func LoadFile(l *State, fileName, mode string) error {
	var f *os.File
	fileNameIndex := l.Top() + 1
	fileError := func(what string) error {
		fileName, _ := l.ToString(fileNameIndex)
		l.PushFString("cannot %s %s", what, fileName[1:])
		l.Remove(fileNameIndex)
		return FileError
	}
	if fileName == "" {
		l.PushString("=stdin")
		f = os.Stdin
	} else {
		l.PushString("@" + fileName)
		var err error
		if f, err = os.Open(fileName); err != nil {
			return fileError("open")
		}
	}
	r := bufio.NewReader(f)
	if skipped, err := skipComment(r); err != nil {
		l.SetTop(fileNameIndex)
		return fileError("read")
	} else if skipped {
		r = bufio.NewReader(io.MultiReader(strings.NewReader("\n"), r))
	}
	s, _ := l.ToString(-1)
	err := l.Load(r, s, mode)
	if f != os.Stdin {
		_ = f.Close()
	}
	switch err {
	case nil, SyntaxError, MemoryError: // do nothing
	default:
		l.SetTop(fileNameIndex)
		return fileError("read")
	}
	l.Remove(fileNameIndex)
	return err
}

func LoadString(l *State, s string) error { return LoadBuffer(l, s, s, "") }

func LoadBuffer(l *State, b, name, mode string) error {
	return l.Load(strings.NewReader(b), name, mode)
}

// NewStateEx creates a new Lua state. It calls NewState and then sets a panic
// function that prints an error message to the standard error output in case
// of fatal errors.
//
// Returns the new state.
func NewStateEx() *State {
	l := NewState()
	if l != nil {
		_ = AtPanic(l, func(l *State) int {
			s, _ := l.ToString(-1)
			fmt.Fprintf(os.Stderr, "PANIC: unprotected error in call to Lua API (%s)\n", s)
			return 0
		})
	}
	return l
}

func LengthEx(l *State, index int) int {
	l.Length(index)
	if length, ok := l.ToInteger(-1); ok {
		l.Pop(1)
		return length
	}
	Errorf(l, "object length is not a number")
	panic("unreachable")
}

// FileResult produces the return values for file-related functions in the standard
// library (io.open, os.rename, file:seek, etc.).
func FileResult(l *State, err error, filename string) int {
	if err == nil {
		l.PushBoolean(true)
		return 1
	}
	l.PushNil()
	if filename != "" {
		l.PushString(filename + ": " + err.Error())
	} else {
		l.PushString(err.Error())
	}
	errno, ok := fileErrorToErrno(err)
	if !ok {
		errno = 0
	}
	l.PushInteger(errno)
	return 3
}

func fileErrorToErrno(err error) (errno int, ok bool) {
	if errors.Is(err, io.ErrClosedPipe) {
		errno, ok = int(int64(uintptr(syscall.EPIPE))), true
	} else {
		switch err.Error() {
		case "EPERM":
			errno, ok = int(int64(uintptr(syscall.EPERM))), true
		case "ENOENT":
			errno, ok = int(int64(uintptr(syscall.ENOENT))), true
		case "ESRCH":
			errno, ok = int(int64(uintptr(syscall.ESRCH))), true
		case "EINTR":
			errno, ok = int(int64(uintptr(syscall.EINTR))), true
		case "EIO":
			errno, ok = int(int64(uintptr(syscall.EIO))), true
		case "ENXIO":
			errno, ok = int(int64(uintptr(syscall.ENXIO))), true
		case "E2BIG":
			errno, ok = int(int64(uintptr(syscall.E2BIG))), true
		case "ENOEXEC":
			errno, ok = int(int64(uintptr(syscall.ENOEXEC))), true
		case "EBADF":
			errno, ok = int(int64(uintptr(syscall.EBADF))), true
		case "ECHILD":
			errno, ok = int(int64(uintptr(syscall.ECHILD))), true
		case "EAGAIN":
			errno, ok = int(int64(uintptr(syscall.EAGAIN))), true
		case "ENOMEM":
			errno, ok = int(int64(uintptr(syscall.ENOMEM))), true
		case "EACCES":
			errno, ok = int(int64(uintptr(syscall.EACCES))), true
		case "EFAULT":
			errno, ok = int(int64(uintptr(syscall.EFAULT))), true
		case "EBUSY":
			errno, ok = int(int64(uintptr(syscall.EBUSY))), true
		case "EEXIST":
			errno, ok = int(int64(uintptr(syscall.EEXIST))), true
		case "EXDEV":
			errno, ok = int(int64(uintptr(syscall.EXDEV))), true
		case "ENODEV":
			errno, ok = int(int64(uintptr(syscall.ENODEV))), true
		case "ENOTDIR":
			errno, ok = int(int64(uintptr(syscall.ENOTDIR))), true
		case "EISDIR":
			errno, ok = int(int64(uintptr(syscall.EISDIR))), true
		case "EINVAL":
			errno, ok = int(int64(uintptr(syscall.EINVAL))), true
		case "ENFILE":
			errno, ok = int(int64(uintptr(syscall.ENFILE))), true
		case "EMFILE":
			errno, ok = int(int64(uintptr(syscall.EMFILE))), true
		case "ENOTTY":
			errno, ok = int(int64(uintptr(syscall.ENOTTY))), true
		case "EFBIG":
			errno, ok = int(int64(uintptr(syscall.EFBIG))), true
		case "ENOSPC":
			errno, ok = int(int64(uintptr(syscall.ENOSPC))), true
		case "ESPIPE":
			errno, ok = int(int64(uintptr(syscall.ESPIPE))), true
		case "EROFS":
			errno, ok = int(int64(uintptr(syscall.EROFS))), true
		case "EMLINK":
			errno, ok = int(int64(uintptr(syscall.EMLINK))), true
		case "EPIPE":
			errno, ok = int(int64(uintptr(syscall.EPIPE))), true
		case "ENAMETOOLONG":
			errno, ok = int(int64(uintptr(syscall.ENAMETOOLONG))), true
		case "ENOSYS":
			errno, ok = int(int64(uintptr(syscall.ENOSYS))), true
		case "EDQUOT":
			errno, ok = int(int64(uintptr(syscall.EDQUOT))), true
		case "EDOM":
			errno, ok = int(int64(uintptr(syscall.EDOM))), true
		case "ERANGE":
			errno, ok = int(int64(uintptr(syscall.ERANGE))), true
		case "EDEADLK":
			errno, ok = int(int64(uintptr(syscall.EDEADLK))), true
		case "ENOLCK":
			errno, ok = int(int64(uintptr(syscall.ENOLCK))), true
		case "ENOTEMPTY":
			errno, ok = int(int64(uintptr(syscall.ENOTEMPTY))), true
		case "ELOOP":
			errno, ok = int(int64(uintptr(syscall.ELOOP))), true
		case "ENOMSG":
			errno, ok = int(int64(uintptr(syscall.ENOMSG))), true
		case "EIDRM":
			errno, ok = int(int64(uintptr(syscall.EIDRM))), true
		case "ECHRNG":
			errno, ok = int(int64(uintptr(syscall.ECHRNG))), true
		case "EL2NSYNC":
			errno, ok = int(int64(uintptr(syscall.EL2NSYNC))), true
		case "EL3HLT":
			errno, ok = int(int64(uintptr(syscall.EL3HLT))), true
		case "EL3RST":
			errno, ok = int(int64(uintptr(syscall.EL3RST))), true
		case "ELNRNG":
			errno, ok = int(int64(uintptr(syscall.ELNRNG))), true
		case "EUNATCH":
			errno, ok = int(int64(uintptr(syscall.EUNATCH))), true
		case "ENOCSI":
			errno, ok = int(int64(uintptr(syscall.ENOCSI))), true
		case "EL2HLT":
			errno, ok = int(int64(uintptr(syscall.EL2HLT))), true
		case "EBADE":
			errno, ok = int(int64(uintptr(syscall.EBADE))), true
		case "EBADR":
			errno, ok = int(int64(uintptr(syscall.EBADR))), true
		case "EXFULL":
			errno, ok = int(int64(uintptr(syscall.EXFULL))), true
		case "ENOANO":
			errno, ok = int(int64(uintptr(syscall.ENOANO))), true
		case "EBADRQC":
			errno, ok = int(int64(uintptr(syscall.EBADRQC))), true
		case "EBADSLT":
			errno, ok = int(int64(uintptr(syscall.EBADSLT))), true
		case "EDEADLOCK":
			errno, ok = int(int64(uintptr(syscall.EDEADLOCK))), true
		case "EBFONT":
			errno, ok = int(int64(uintptr(syscall.EBFONT))), true
		case "ENOSTR":
			errno, ok = int(int64(uintptr(syscall.ENOSTR))), true
		case "ENODATA":
			errno, ok = int(int64(uintptr(syscall.ENODATA))), true
		case "ETIME":
			errno, ok = int(int64(uintptr(syscall.ETIME))), true
		case "ENOSR":
			errno, ok = int(int64(uintptr(syscall.ENOSR))), true
		case "ENONET":
			errno, ok = int(int64(uintptr(syscall.ENONET))), true
		case "ENOPKG":
			errno, ok = int(int64(uintptr(syscall.ENOPKG))), true
		case "EREMOTE":
			errno, ok = int(int64(uintptr(syscall.EREMOTE))), true
		case "ENOLINK":
			errno, ok = int(int64(uintptr(syscall.ENOLINK))), true
		case "EADV":
			errno, ok = int(int64(uintptr(syscall.EADV))), true
		case "ESRMNT":
			errno, ok = int(int64(uintptr(syscall.ESRMNT))), true
		case "ECOMM":
			errno, ok = int(int64(uintptr(syscall.ECOMM))), true
		case "EPROTO":
			errno, ok = int(int64(uintptr(syscall.EPROTO))), true
		case "EMULTIHOP":
			errno, ok = int(int64(uintptr(syscall.EMULTIHOP))), true
		case "EDOTDOT":
			errno, ok = int(int64(uintptr(syscall.EDOTDOT))), true
		case "EBADMSG":
			errno, ok = int(int64(uintptr(syscall.EBADMSG))), true
		case "EOVERFLOW":
			errno, ok = int(int64(uintptr(syscall.EOVERFLOW))), true
		case "ENOTUNIQ":
			errno, ok = int(int64(uintptr(syscall.ENOTUNIQ))), true
		case "EBADFD":
			errno, ok = int(int64(uintptr(syscall.EBADFD))), true
		case "EREMCHG":
			errno, ok = int(int64(uintptr(syscall.EREMCHG))), true
		case "ELIBACC":
			errno, ok = int(int64(uintptr(syscall.ELIBACC))), true
		case "ELIBBAD":
			errno, ok = int(int64(uintptr(syscall.ELIBBAD))), true
		case "ELIBSCN":
			errno, ok = int(int64(uintptr(syscall.ELIBSCN))), true
		case "ELIBMAX":
			errno, ok = int(int64(uintptr(syscall.ELIBMAX))), true
		case "ELIBEXEC":
			errno, ok = int(int64(uintptr(syscall.ELIBEXEC))), true
		case "EILSEQ":
			errno, ok = int(int64(uintptr(syscall.EILSEQ))), true
		case "EUSERS":
			errno, ok = int(int64(uintptr(syscall.EUSERS))), true
		case "ENOTSOCK":
			errno, ok = int(int64(uintptr(syscall.ENOTSOCK))), true
		case "EDESTADDRREQ":
			errno, ok = int(int64(uintptr(syscall.EDESTADDRREQ))), true
		case "EMSGSIZE":
			errno, ok = int(int64(uintptr(syscall.EMSGSIZE))), true
		case "EPROTOTYPE":
			errno, ok = int(int64(uintptr(syscall.EPROTOTYPE))), true
		case "ENOPROTOOPT":
			errno, ok = int(int64(uintptr(syscall.ENOPROTOOPT))), true
		case "EPROTONOSUPPORT":
			errno, ok = int(int64(uintptr(syscall.EPROTONOSUPPORT))), true
		case "ESOCKTNOSUPPORT":
			errno, ok = int(int64(uintptr(syscall.ESOCKTNOSUPPORT))), true
		case "EOPNOTSUPP":
			errno, ok = int(int64(uintptr(syscall.EOPNOTSUPP))), true
		case "EPFNOSUPPORT":
			errno, ok = int(int64(uintptr(syscall.EPFNOSUPPORT))), true
		case "EAFNOSUPPORT":
			errno, ok = int(int64(uintptr(syscall.EAFNOSUPPORT))), true
		case "EADDRINUSE":
			errno, ok = int(int64(uintptr(syscall.EADDRINUSE))), true
		case "EADDRNOTAVAIL":
			errno, ok = int(int64(uintptr(syscall.EADDRNOTAVAIL))), true
		case "ENETDOWN":
			errno, ok = int(int64(uintptr(syscall.ENETDOWN))), true
		case "ENETUNREACH":
			errno, ok = int(int64(uintptr(syscall.ENETUNREACH))), true
		case "ENETRESET":
			errno, ok = int(int64(uintptr(syscall.ENETRESET))), true
		case "ECONNABORTED":
			errno, ok = int(int64(uintptr(syscall.ECONNABORTED))), true
		case "ECONNRESET":
			errno, ok = int(int64(uintptr(syscall.ECONNRESET))), true
		case "ENOBUFS":
			errno, ok = int(int64(uintptr(syscall.ENOBUFS))), true
		case "EISCONN":
			errno, ok = int(int64(uintptr(syscall.EISCONN))), true
		case "ENOTCONN":
			errno, ok = int(int64(uintptr(syscall.ENOTCONN))), true
		case "ESHUTDOWN":
			errno, ok = int(int64(uintptr(syscall.ESHUTDOWN))), true
		case "ETOOMANYREFS":
			errno, ok = int(int64(uintptr(syscall.ETOOMANYREFS))), true
		case "ETIMEDOUT":
			errno, ok = int(int64(uintptr(syscall.ETIMEDOUT))), true
		case "ECONNREFUSED":
			errno, ok = int(int64(uintptr(syscall.ECONNREFUSED))), true
		case "EHOSTDOWN":
			errno, ok = int(int64(uintptr(syscall.EHOSTDOWN))), true
		case "EHOSTUNREACH":
			errno, ok = int(int64(uintptr(syscall.EHOSTUNREACH))), true
		case "EALREADY":
			errno, ok = int(int64(uintptr(syscall.EALREADY))), true
		case "EINPROGRESS":
			errno, ok = int(int64(uintptr(syscall.EINPROGRESS))), true
		case "ESTALE":
			errno, ok = int(int64(uintptr(syscall.ESTALE))), true
		case "ENOTSUP":
			errno, ok = int(int64(uintptr(syscall.ENOTSUP))), true
		case "ENOMEDIUM":
			errno, ok = int(int64(uintptr(syscall.ENOMEDIUM))), true
		case "ECANCELED":
			errno, ok = int(int64(uintptr(syscall.ECANCELED))), true
		case "EWOULDBLOCK":
			errno, ok = int(int64(uintptr(syscall.EWOULDBLOCK))), true
		}
	}
	return
}

// DoFile loads and runs the given file.
func DoFile(l *State, fileName string) error {
	if err := LoadFile(l, fileName, ""); err != nil {
		return err
	}
	return l.ProtectedCall(0, MultipleReturns, 0)
}

// DoString loads and runs the given string.
func DoString(l *State, s string) error {
	if err := LoadString(l, s); err != nil {
		return err
	}
	return l.ProtectedCall(0, MultipleReturns, 0)
}
