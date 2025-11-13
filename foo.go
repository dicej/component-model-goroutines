package main

import (
	"fmt"
	"runtime"
	"unsafe"
)

func Foo(value string) string {
	channel := make(chan string)
	callImport := func() {
		channel <- ImportFoo(value)
	}
	go callImport()
	go callImport()
	go callImport()
	return fmt.Sprintf("guest[%v, %v, %v]", <-channel, <-channel, <-channel)
}

// The remaining code would normally be either generated automatically from the
// WIT file or packaged as part of a component model runtime library.  Here we
// define it manually as part of this proof-of-concept:

const EVENT_NONE uint32 = 0
const EVENT_SUBTASK uint32 = 1

const STATUS_STARTING uint32 = 0
const STATUS_STARTED uint32 = 1
const STATUS_RETURNED uint32 = 2

const CALLBACK_CODE_EXIT uint32 = 0
const CALLBACK_CODE_WAIT uint32 = 2

type idle struct{}

type futureState struct {
	channel     chan idle
	waitableSet uint32
	pending     map[uint32]chan uint32
	pinner      runtime.Pinner
}

var state *futureState = nil

//go:wasmexport [async-lift]local:local/baz#[async]foo
func exportFoo(utf8 unsafe.Pointer, length uint32) uint32 {
	state = &futureState{make(chan idle), 0, make(map[uint32]chan uint32), runtime.Pinner{}}
	state.pinner.Pin(state)

	defer func() {
		state = nil
	}()

	go func() {
		result := Foo(unsafe.String((*uint8)(utf8), length))
		taskReturnFoo(unsafe.StringData(result), uint32(len(result)))
	}()

	return callback(EVENT_NONE, 0, 0)
}

//go:wasmexport [callback][async-lift]local:local/baz#[async]foo
func callbackFoo(event0 uint32, event1 uint32, event2 uint32) uint32 {
	state = (*futureState)(contextGet())
	contextSet(nil)

	return callback(event0, event1, event2)
}

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
func importFoo(utf8 *uint8, length uint32, results unsafe.Pointer) uint32

func ImportFoo(value string) string {
	const RESULT_COUNT uint32 = 2

	pinner := runtime.Pinner{}
	defer pinner.Unpin()

	utf8 := unsafe.StringData(value)
	pinner.Pin(utf8)

	results := unsafe.Pointer(unsafe.SliceData(make([]uint32, RESULT_COUNT)))
	pinner.Pin(results)

	status := importFoo(utf8, uint32(len(value)), results)

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

//go:wasmimport [export]local:local/baz [task-return][async]foo
func taskReturnFoo(utf8 *uint8, length uint32)

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
