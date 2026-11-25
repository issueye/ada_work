//go:build windows

package shelllocal

import (
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobs 记录 pid → Job Object 句柄:用于终止整棵进程树,并在结束时关闭句柄。
var jobs sync.Map

// configure 目前无平台特定设置(Job Object 在 Start 之后分配)。
func configure(cmd *exec.Cmd) {}

// afterStart 把子进程放进 Job Object,并设置「句柄关闭即杀死全部进程」。
func afterStart(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	proc, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	defer windows.CloseHandle(proc)
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	jobs.Store(uint32(cmd.Process.Pid), job)
	return nil
}

// terminate 终止 Job Object 里的整棵进程树;没有句柄时退回杀父进程。
func terminate(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if v, ok := jobs.Load(uint32(cmd.Process.Pid)); ok {
		if h, ok := v.(windows.Handle); ok {
			_ = windows.TerminateJobObject(h, 1)
			_ = windows.CloseHandle(h)
		}
		jobs.Delete(uint32(cmd.Process.Pid))
	}
	_ = cmd.Process.Kill()
}

// cleanup 关闭 Job 句柄(KILL_ON_JOB_CLOSE 保证残留子进程也被终止)。
func cleanup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if v, ok := jobs.Load(uint32(cmd.Process.Pid)); ok {
		if h, ok := v.(windows.Handle); ok {
			_ = windows.CloseHandle(h)
		}
		jobs.Delete(uint32(cmd.Process.Pid))
	}
}
