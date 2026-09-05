//go:build windows

package memory

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

var (
	psapi                    = windows.NewLazySystemDLL("psapi.dll")
	getProcessMemoryCounters = psapi.NewProc("GetProcessMemoryInfo")
)

func RSS() (uint64, error) {
	proc, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(os.Getpid()))
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(proc)
	counters := processMemoryCounters{cb: uint32(unsafe.Sizeof(processMemoryCounters{}))}
	ret, _, callErr := getProcessMemoryCounters.Call(uintptr(proc), uintptr(unsafe.Pointer(&counters)), uintptr(counters.cb))
	if ret == 0 {
		if callErr != nil && callErr != windows.ERROR_SUCCESS {
			return 0, callErr
		}
		return 0, fmt.Errorf("GetProcessMemoryInfo failed")
	}
	return uint64(counters.workingSetSize), nil
}
