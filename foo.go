package main

import (
	"fmt"
	"runtime"
	"unsafe"
)

func Foo(value string, delay bool) string {
	channel := make(chan string)
	for i := 0; i < 3; i++ {
		go func() {
			channel <- ImportFoo(value, delay)
		}()
	}
	return fmt.Sprintf("guest[%v, %v, %v]", <-channel, <-channel, <-channel)
}

func ReadStreamU8(stream StreamReader[uint8]) []uint8 {
	defer stream.Drop()

	result := make([]uint8, 0)
	for !stream.WriterDropped() {
		result = append(result, stream.Read(16*1024)...)
	}
	return result
}

func EchoStreamU8(stream StreamReader[uint8]) StreamReader[uint8] {
	tx, rx := MakeStreamU8()

	go func() {
		defer stream.Drop()
		defer tx.Drop()

		for !(stream.WriterDropped() || tx.ReaderDropped()) {
			tx.WriteAll(stream.Read(16 * 1024))
		}
	}()

	return rx
}

func EchoFutureString(future FutureReader[string]) FutureReader[string] {
	tx, rx := MakeFutureString()

	go func() {
		defer future.Drop()
		defer tx.Drop()

		tx.Write(future.Read())
	}()

	return rx
}

// The remaining code would normally be either generated automatically from the
// WIT file or packaged as part of a component model runtime library.  Here we
// define it manually as part of this proof-of-concept:

const EVENT_NONE uint32 = 0
const EVENT_SUBTASK uint32 = 1
const EVENT_STREAM_READ uint32 = 2
const EVENT_STREAM_WRITE uint32 = 3
const EVENT_FUTURE_READ uint32 = 4
const EVENT_FUTURE_WRITE uint32 = 5

const STATUS_STARTING uint32 = 0
const STATUS_STARTED uint32 = 1
const STATUS_RETURNED uint32 = 2

const CALLBACK_CODE_EXIT uint32 = 0
const CALLBACK_CODE_WAIT uint32 = 2

const RETURN_CODE_BLOCKED uint32 = 0xFFFFFFFF
const RETURN_CODE_COMPLETED uint32 = 0
const RETURN_CODE_DROPPED uint32 = 1

type StreamVtable[T any] struct {
	size         uint32
	align        uint32
	read         func(handle uint32, items unsafe.Pointer, length uint32) uint32
	write        func(handle uint32, items unsafe.Pointer, length uint32) uint32
	cancelRead   func(handle uint32) uint32
	cancelWrite  func(handle uint32) uint32
	dropReadable func(handle uint32)
	dropWritable func(handle uint32)
	lift         func(src unsafe.Pointer) T
	lower        func(value T, dst unsafe.Pointer)
}

type StreamReader[T any] struct {
	vtable        *StreamVtable[T]
	handle        uint32
	writerDropped bool
}

func (s *StreamReader[T]) WriterDropped() bool {
	return s.writerDropped
}

func (s *StreamReader[T]) Read(maxCount uint32) []T {
	if s.handle == 0 {
		panic("null stream handle")
	}

	if s.writerDropped {
		return []T{}
	}

	pinner := runtime.Pinner{}
	defer pinner.Unpin()

	buffer := cabiRealloc(nil, 0, uintptr(s.vtable.align), uintptr(s.vtable.size*maxCount))
	pinner.Pin(buffer)

	code := s.vtable.read(s.handle, buffer, maxCount)

	if code == RETURN_CODE_BLOCKED {
		if state.waitableSet == 0 {
			state.waitableSet = waitableSetNew()
		}
		waitableJoin(s.handle, state.waitableSet)
		channel := make(chan uint32)
		state.pending[s.handle] = channel
		code = (<-channel)
	}

	count := code >> 4
	code = code & 0xF

	if code == RETURN_CODE_DROPPED {
		s.writerDropped = true
	}

	if s.vtable.lift == nil {
		return unsafe.Slice((*T)(buffer), count)
	} else {
		result := make([]T, 0, count)
		for i := 0; i < int(count); i++ {
			result = append(result, s.vtable.lift(unsafe.Add(buffer, i*int(s.vtable.size))))
		}
		return result
	}
}

func (s *StreamReader[T]) Drop() {
	handle := s.handle
	if handle != 0 {
		s.handle = 0
		s.vtable.dropReadable(handle)
	}
}

func (s *StreamReader[T]) TakeHandle() uint32 {
	if s.handle == 0 {
		panic("null stream handle")
	}

	handle := s.handle
	s.handle = 0
	return handle
}

type StreamWriter[T any] struct {
	vtable        *StreamVtable[T]
	handle        uint32
	readerDropped bool
}

func (s *StreamWriter[T]) ReaderDropped() bool {
	return s.readerDropped
}

func (s *StreamWriter[T]) Write(items []T) uint32 {
	if s.handle == 0 {
		panic("null stream handle")
	}

	if s.readerDropped {
		return 0
	}

	pinner := runtime.Pinner{}
	defer pinner.Unpin()

	writeCount := uint32(len(items))

	var buffer unsafe.Pointer
	if s.vtable.lower == nil {
		buffer = unsafe.Pointer(unsafe.SliceData(items))
	} else {
		buffer = cabiRealloc(nil, 0, uintptr(s.vtable.align), uintptr(s.vtable.size*writeCount))
		for index, item := range items {
			s.vtable.lower(item, unsafe.Add(buffer, index*int(s.vtable.size)))
		}
	}
	pinner.Pin(buffer)

	code := s.vtable.write(s.handle, buffer, writeCount)

	if code == RETURN_CODE_BLOCKED {
		if state.waitableSet == 0 {
			state.waitableSet = waitableSetNew()
		}
		waitableJoin(s.handle, state.waitableSet)
		channel := make(chan uint32)
		state.pending[s.handle] = channel
		code = (<-channel)
	}

	count := code >> 4
	code = code & 0xF

	if code == RETURN_CODE_DROPPED {
		s.readerDropped = true
	}

	return count
}

func (s *StreamWriter[T]) WriteAll(items []T) uint32 {
	offset := uint32(0)
	count := uint32(len(items))
	for offset < count && !s.readerDropped {
		offset += s.Write(items[offset:])
	}
	return offset
}

func (s *StreamWriter[T]) Drop() {
	handle := s.handle
	if handle != 0 {
		s.handle = 0
		s.vtable.dropWritable(handle)
	}
}

type FutureVtable[T any] struct {
	size         uint32
	align        uint32
	read         func(handle uint32, item unsafe.Pointer) uint32
	write        func(handle uint32, item unsafe.Pointer) uint32
	cancelRead   func(handle uint32) uint32
	cancelWrite  func(handle uint32) uint32
	dropReadable func(handle uint32)
	dropWritable func(handle uint32)
	lift         func(src unsafe.Pointer) T
	lower        func(value T, dst unsafe.Pointer)
}

type FutureReader[T any] struct {
	vtable *FutureVtable[T]
	handle uint32
}

func (f *FutureReader[T]) Read() T {
	if f.handle == 0 {
		panic("null future handle")
	}

	handle := f.handle
	f.handle = 0
	defer f.vtable.dropReadable(handle)

	pinner := runtime.Pinner{}
	defer pinner.Unpin()

	buffer := cabiRealloc(nil, 0, uintptr(f.vtable.align), uintptr(f.vtable.size))
	pinner.Pin(buffer)

	code := f.vtable.read(handle, buffer)

	if code == RETURN_CODE_BLOCKED {
		if state.waitableSet == 0 {
			state.waitableSet = waitableSetNew()
		}
		waitableJoin(handle, state.waitableSet)
		channel := make(chan uint32)
		state.pending[handle] = channel
		code = (<-channel)
	}

	code = code & 0xF

	switch code {
	case RETURN_CODE_COMPLETED, RETURN_CODE_DROPPED:
		if f.vtable.lift == nil {
			return unsafe.Slice((*T)(buffer), 1)[0]
		} else {
			return f.vtable.lift(buffer)
		}

	default:
		panic("todo: handle cancellation")
	}
}

func (f *FutureReader[T]) Drop() {
	handle := f.handle
	if handle != 0 {
		f.handle = 0
		f.vtable.dropReadable(handle)
	}
}

func (f *FutureReader[T]) TakeHandle() uint32 {
	if f.handle == 0 {
		panic("null future handle")
	}

	handle := f.handle
	f.handle = 0
	return handle
}

type FutureWriter[T any] struct {
	vtable *FutureVtable[T]
	handle uint32
}

func (f *FutureWriter[T]) Write(item T) bool {
	if f.handle == 0 {
		panic("null future handle")
	}

	handle := f.handle
	f.handle = 0
	defer f.vtable.dropWritable(handle)

	pinner := runtime.Pinner{}
	defer pinner.Unpin()

	var buffer unsafe.Pointer
	if f.vtable.lower == nil {
		buffer = unsafe.Pointer(unsafe.SliceData([]T{item}))
	} else {
		buffer = cabiRealloc(nil, 0, uintptr(f.vtable.align), uintptr(f.vtable.size))
		f.vtable.lower(item, buffer)
	}
	pinner.Pin(buffer)

	code := f.vtable.write(handle, buffer)

	if code == RETURN_CODE_BLOCKED {
		if state.waitableSet == 0 {
			state.waitableSet = waitableSetNew()
		}
		waitableJoin(handle, state.waitableSet)
		channel := make(chan uint32)
		state.pending[handle] = channel
		code = (<-channel)
	}

	code = code & 0xF

	switch code {
	case RETURN_CODE_COMPLETED:
		return true

	case RETURN_CODE_DROPPED:
		return false

	default:
		panic("todo: handle cancellation")
	}
}

func (f *FutureWriter[T]) Drop() {
	handle := f.handle
	if handle != 0 {
		f.handle = 0
		f.vtable.dropWritable(handle)
	}
}

type idle struct{}

type taskState struct {
	channel     chan idle
	waitableSet uint32
	pending     map[uint32]chan uint32
	pinner      runtime.Pinner
}

var state *taskState = nil

//go:wasmexport [async-lift]local:local/baz#[async]foo
func exportFoo(utf8 unsafe.Pointer, length uint32, delay bool) uint32 {
	state = &taskState{
		make(chan idle),
		0,
		make(map[uint32]chan uint32),
		runtime.Pinner{},
	}
	state.pinner.Pin(state)

	defer func() {
		state = nil
	}()

	go func() {
		result := Foo(unsafe.String((*uint8)(utf8), length), delay)
		taskReturnFoo(unsafe.StringData(result), uint32(len(result)))
	}()

	return callback(EVENT_NONE, 0, 0)
}

//go:wasmexport [callback][async-lift]local:local/baz#[async]foo
func callbackFoo(event0 uint32, event1 uint32, event2 uint32) uint32 {
	state = (*taskState)(contextGet())
	contextSet(nil)

	return callback(event0, event1, event2)
}

//go:wasmimport [export]local:local/baz [task-return][async]foo
func taskReturnFoo(utf8 *uint8, length uint32)

//go:wasmimport [export]local:local/streams-and-futures [stream-new-0][async]read-stream-u8
func streamU8New() uint64

//go:wasmimport [export]local:local/streams-and-futures [async-lower][stream-read-0][async]read-stream-u8
func streamU8Read(handle uint32, items unsafe.Pointer, length uint32) uint32

//go:wasmimport [export]local:local/streams-and-futures [async-lower][stream-write-0][async]read-stream-u8
func streamU8Write(handle uint32, items unsafe.Pointer, length uint32) uint32

//go:wasmimport [export]local:local/streams-and-futures [stream-drop-readable-0][async]read-stream-u8
func streamU8DropReadable(handle uint32)

//go:wasmimport [export]local:local/streams-and-futures [stream-drop-writable-0][async]read-stream-u8
func streamU8DropWritable(handle uint32)

var streamU8Vtable = StreamVtable[uint8]{
	1,
	1,
	streamU8Read,
	streamU8Write,
	nil,
	nil,
	streamU8DropReadable,
	streamU8DropWritable,
	nil,
	nil,
}

func MakeStreamU8() (StreamWriter[uint8], StreamReader[uint8]) {
	pair := streamU8New()
	return StreamWriter[uint8]{&streamU8Vtable, uint32(pair >> 32), false},
		StreamReader[uint8]{&streamU8Vtable, uint32(pair & 0xFFFFFFFF), true}
}

//go:wasmexport [async-lift]local:local/streams-and-futures#[async]read-stream-u8
func exportReadStreamU8(stream uint32) uint32 {
	state = &taskState{
		make(chan idle),
		0,
		make(map[uint32]chan uint32),
		runtime.Pinner{},
	}
	state.pinner.Pin(state)

	defer func() {
		state = nil
	}()

	go func() {
		result := ReadStreamU8(StreamReader[uint8]{&streamU8Vtable, stream, false})
		taskReturnReadStreamU8(unsafe.SliceData(result), uint32(len(result)))
	}()

	return callback(EVENT_NONE, 0, 0)
}

//go:wasmexport [callback][async-lift]local:local/streams-and-futures#[async]read-stream-u8
func callbackReadStreamU8(event0 uint32, event1 uint32, event2 uint32) uint32 {
	state = (*taskState)(contextGet())
	contextSet(nil)

	return callback(event0, event1, event2)
}

//go:wasmimport [export]local:local/streams-and-futures [task-return][async]read-stream-u8
func taskReturnReadStreamU8(list *uint8, length uint32)

//go:wasmexport [async-lift]local:local/streams-and-futures#[async]echo-stream-u8
func exportEchoStreamU8(stream uint32) uint32 {
	state = &taskState{
		make(chan idle),
		0,
		make(map[uint32]chan uint32),
		runtime.Pinner{},
	}
	state.pinner.Pin(state)

	defer func() {
		state = nil
	}()

	go func() {
		result := EchoStreamU8(StreamReader[uint8]{&streamU8Vtable, stream, false})
		taskReturnEchoStreamU8(result.TakeHandle())
	}()

	return callback(EVENT_NONE, 0, 0)
}

//go:wasmexport [callback][async-lift]local:local/streams-and-futures#[async]echo-stream-u8
func callbackEchoStreamU8(event0 uint32, event1 uint32, event2 uint32) uint32 {
	state = (*taskState)(contextGet())
	contextSet(nil)

	return callback(event0, event1, event2)
}

//go:wasmimport [export]local:local/streams-and-futures [task-return][async]echo-stream-u8
func taskReturnEchoStreamU8(handle uint32)

//go:wasmimport [export]local:local/streams-and-futures [future-new-0][async]echo-future-string
func futureStringNew() uint64

//go:wasmimport [export]local:local/streams-and-futures [async-lower][future-read-0][async]echo-future-string
func futureStringRead(handle uint32, item unsafe.Pointer) uint32

//go:wasmimport [export]local:local/streams-and-futures [async-lower][future-write-0][async]echo-future-string
func futureStringWrite(handle uint32, item unsafe.Pointer) uint32

//go:wasmimport [export]local:local/streams-and-futures [future-drop-readable-0][async]echo-future-string
func futureStringDropReadable(handle uint32)

//go:wasmimport [export]local:local/streams-and-futures [future-drop-writable-0][async]echo-future-string
func futureStringDropWritable(handle uint32)

func futureStringLift(src unsafe.Pointer) string {
	return unsafe.String(unsafe.Slice((**uint8)(src), 2)[0], unsafe.Slice((*uint32)(src), 2)[1])
}

func futureStringLower(value string, dst unsafe.Pointer) {
	unsafe.Slice((**uint8)(dst), 2)[0] = unsafe.StringData(value)
	unsafe.Slice((*uint32)(dst), 2)[1] = uint32(len(value))
}

var futureStringVtable = FutureVtable[string]{
	8,
	4,
	futureStringRead,
	futureStringWrite,
	nil,
	nil,
	futureStringDropReadable,
	futureStringDropWritable,
	futureStringLift,
	futureStringLower,
}

func MakeFutureString() (FutureWriter[string], FutureReader[string]) {
	pair := futureStringNew()
	return FutureWriter[string]{&futureStringVtable, uint32(pair >> 32)},
		FutureReader[string]{&futureStringVtable, uint32(pair & 0xFFFFFFFF)}
}

//go:wasmexport [async-lift]local:local/streams-and-futures#[async]echo-future-string
func exportEchoFutureString(future uint32) uint32 {
	state = &taskState{
		make(chan idle),
		0,
		make(map[uint32]chan uint32),
		runtime.Pinner{},
	}
	state.pinner.Pin(state)

	defer func() {
		state = nil
	}()

	go func() {
		result := EchoFutureString(FutureReader[string]{&futureStringVtable, future})
		taskReturnEchoFutureString(result.TakeHandle())
	}()

	return callback(EVENT_NONE, 0, 0)
}

//go:wasmexport [callback][async-lift]local:local/streams-and-futures#[async]echo-future-string
func callbackEchoFutureString(event0 uint32, event1 uint32, event2 uint32) uint32 {
	state = (*taskState)(contextGet())
	contextSet(nil)

	return callback(event0, event1, event2)
}

//go:wasmimport [export]local:local/streams-and-futures [task-return][async]echo-future-string
func taskReturnEchoFutureString(handle uint32)

func callback(event0 uint32, event1 uint32, event2 uint32) uint32 {
	switch event0 {
	case EVENT_NONE:

	case EVENT_SUBTASK:
		switch event2 {
		case STATUS_STARTING:
			panic(fmt.Sprintf("unexpected subtask status: %v", event2))

		case STATUS_STARTED:

		case STATUS_RETURNED:
			waitableJoin(event1, 0)
			subtaskDrop(event1)
			channel := state.pending[event1]
			delete(state.pending, event1)
			channel <- event2

		default:
			panic("todo")
		}

	case EVENT_STREAM_READ, EVENT_STREAM_WRITE, EVENT_FUTURE_READ, EVENT_FUTURE_WRITE:
		waitableJoin(event1, 0)
		channel := state.pending[event1]
		delete(state.pending, event1)
		channel <- event2

	default:
		panic("todo")
	}

	// Tell the Go scheduler to write to `state.channel` only after all
	// goroutines have either blocked or exited.  This allows us to reliably
	// delay returning control to the host until there's truly nothing more
	// we can do in the guest.
	runtime.WasiOnIdle(func() bool {
		state.channel <- idle{}
		return true
	})
	defer runtime.WasiOnIdle(func() bool {
		return false
	})

	// Block this goroutine until the scheduler wakes us up.
	(<-state.channel)

	if len(state.pending) == 0 {
		state.pinner.Unpin()
		if state.waitableSet != 0 {
			waitableSetDrop(state.waitableSet)
		}
		return CALLBACK_CODE_EXIT
	} else {
		if state.waitableSet == 0 {
			panic("unreachable")
		}
		contextSet(unsafe.Pointer(state))
		return CALLBACK_CODE_WAIT | (state.waitableSet << 4)
	}
}

//go:wasmimport local:local/baz [async-lower][async]foo
func importFoo(utf8 *uint8, length uint32, delay bool, results unsafe.Pointer) uint32

func ImportFoo(value string, delay bool) string {
	const RESULT_COUNT uint32 = 2

	pinner := runtime.Pinner{}
	defer pinner.Unpin()

	utf8 := unsafe.StringData(value)
	pinner.Pin(utf8)

	results := unsafe.Pointer(unsafe.SliceData(make([]uint32, RESULT_COUNT)))
	pinner.Pin(results)

	status := importFoo(utf8, uint32(len(value)), delay, results)

	subtask := status >> 4
	status = status & 0xF

	switch status {
	case STATUS_STARTING, STATUS_STARTED:
		if state.waitableSet == 0 {
			state.waitableSet = waitableSetNew()
		}
		waitableJoin(subtask, state.waitableSet)
		channel := make(chan uint32)
		state.pending[subtask] = channel
		(<-channel)

	case STATUS_RETURNED:

	default:
		panic(fmt.Sprintf("unexpected subtask status: %v", status))
	}

	return unsafe.String(
		unsafe.Slice((**uint8)(results), RESULT_COUNT)[0],
		unsafe.Slice((*uint32)(results), RESULT_COUNT)[1],
	)
}

//go:wasmimport $root [waitable-set-new]
func waitableSetNew() uint32

//go:wasmimport $root [waitable-set-drop]
func waitableSetDrop(waitableSet uint32)

//go:wasmimport $root [waitable-join]
func waitableJoin(waitable, waitableSet uint32)

//go:wasmimport $root [context-get-0]
func contextGet() unsafe.Pointer

//go:wasmimport $root [context-set-0]
func contextSet(value unsafe.Pointer)

//go:wasmimport $root [subtask-drop]
func subtaskDrop(subtask uint32)

// Unused, but required to make the compiler happy:
func main() {}

// NB: `cabi_realloc` may be called before the Go runtime has been initialized,
// in which case we need to use `runtime.sbrk` to do allocations.  The following
// is an abbreviation of [Till's
// efforts](https://github.com/bytecodealliance/go-modules/pull/367).

//go:linkname sbrk runtime.sbrk
func sbrk(n uintptr) unsafe.Pointer

var useGCAllocations = false

func init() {
	useGCAllocations = true
}

func offset(ptr, align uintptr) uintptr {
	newptr := (ptr + align - 1) &^ (align - 1)
	return newptr - ptr
}

func ceiling(n, d uintptr) uintptr {
	var hasRemainder uintptr
	if n%d == 0 {
		hasRemainder = 0
	} else {
		hasRemainder = 1
	}
	return n/d + hasRemainder
}

//go:wasmexport cabi_realloc
func cabiRealloc(oldPointer unsafe.Pointer, oldSize, align, newSize uintptr) unsafe.Pointer {
	if oldPointer != nil || oldSize != 0 {
		panic("todo")
	}

	if useGCAllocations {
		switch align {
		case 1:
			return unsafe.Pointer(unsafe.SliceData(make([]uint8, newSize)))
		case 2:
			return unsafe.Pointer(unsafe.SliceData(make([]uint16, ceiling(newSize, align))))
		case 4:
			return unsafe.Pointer(unsafe.SliceData(make([]uint32, ceiling(newSize, align))))
		case 8:
			return unsafe.Pointer(unsafe.SliceData(make([]uint64, ceiling(newSize, align))))
		default:
			panic(fmt.Sprintf("unsupported alignment: %v", align))
		}
	} else {
		alignedSize := newSize + offset(newSize, align)
		unaligned := sbrk(alignedSize)
		off := offset(uintptr(unaligned), align)
		return unsafe.Add(unaligned, off)
	}
}
