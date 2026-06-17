//go:build windows

package updatercore

import (
	"os"
	"syscall"
	"time"
	"unsafe"
)

const (
	errorMoreData     = 234
	rmMaxAppName      = 255
	rmMaxSvcName      = 63
	rmSessionKeyBytes = 32
)

type rmUniqueProcess struct {
	ProcessID        uint32
	ProcessStartTime syscall.Filetime
}

type rmProcessInfo struct {
	Process          rmUniqueProcess
	AppName          [rmMaxAppName + 1]uint16
	ServiceShortName [rmMaxSvcName + 1]uint16
	ApplicationType  uint32
	AppStatus        uint32
	TSSessionID      uint32
	Restartable      int32
}

var (
	rstrtmgr               = syscall.NewLazyDLL("rstrtmgr.dll")
	procRmStartSession     = rstrtmgr.NewProc("RmStartSession")
	procRmRegisterResource = rstrtmgr.NewProc("RmRegisterResources")
	procRmGetList          = rstrtmgr.NewProc("RmGetList")
	procRmEndSession       = rstrtmgr.NewProc("RmEndSession")
)

func FindLockedFiles(paths []string) ([]LockedFile, error) {
	var locked []LockedFile
	for _, path := range paths {
		files, err := findLockedFile(path)
		if err != nil {
			return nil, err
		}
		locked = append(locked, files...)
	}
	return locked, nil
}

func TerminateLockedProcesses(files []LockedFile) error {
	seen := map[int]bool{}
	for _, file := range files {
		if file.PID <= 0 || file.PID == os.Getpid() || seen[file.PID] {
			continue
		}
		seen[file.PID] = true
		proc, err := os.FindProcess(file.PID)
		if err != nil {
			return err
		}
		if err := proc.Kill(); err != nil {
			return err
		}
	}
	time.Sleep(800 * time.Millisecond)
	return nil
}

func findLockedFile(path string) ([]LockedFile, error) {
	var handle uint32
	key, err := syscall.UTF16FromString(sessionKey())
	if err != nil {
		return nil, err
	}
	ret, _, _ := procRmStartSession.Call(uintptr(unsafe.Pointer(&handle)), 0, uintptr(unsafe.Pointer(&key[0])))
	if ret != 0 {
		return nil, nil
	}
	defer procRmEndSession.Call(uintptr(handle))

	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	pathPtrs := []*uint16{pathPtr}
	ret, _, _ = procRmRegisterResource.Call(uintptr(handle), 1, uintptr(unsafe.Pointer(&pathPtrs[0])), 0, 0, 0, 0)
	if ret != 0 {
		return nil, nil
	}

	var needed uint32
	var count uint32
	var reason uint32
	ret, _, _ = procRmGetList.Call(uintptr(handle), uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)), 0, uintptr(unsafe.Pointer(&reason)))
	if ret == 0 {
		return nil, nil
	}
	if ret != errorMoreData || needed == 0 {
		return nil, nil
	}

	infos := make([]rmProcessInfo, needed)
	count = needed
	ret, _, _ = procRmGetList.Call(uintptr(handle), uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&infos[0])), uintptr(unsafe.Pointer(&reason)))
	if ret != 0 {
		return nil, nil
	}
	result := make([]LockedFile, 0, count)
	for i := uint32(0); i < count; i++ {
		pid := int(infos[i].Process.ProcessID)
		if pid == os.Getpid() {
			continue
		}
		result = append(result, LockedFile{
			Path:        path,
			ProcessName: utf16ArrayToString(infos[i].AppName[:]),
			PID:         pid,
		})
	}
	return result, nil
}

func utf16ArrayToString(value []uint16) string {
	end := 0
	for end < len(value) && value[end] != 0 {
		end++
	}
	return syscall.UTF16ToString(value[:end])
}

func sessionKey() string {
	value := time.Now().UnixNano()
	key := "UgeminiUpdater"
	for len(key) < rmSessionKeyBytes {
		key += "0"
	}
	return key[:rmSessionKeyBytes-16] + formatHex(value)
}

func formatHex(value int64) string {
	const digits = "0123456789abcdef"
	var buf [16]byte
	u := uint64(value)
	for i := len(buf) - 1; i >= 0; i-- {
		buf[i] = digits[u&0xf]
		u >>= 4
	}
	return string(buf[:])
}
