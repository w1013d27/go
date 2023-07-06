// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"unsafe"
)

type mOS struct {
	initialized bool
	mutex       pthreadmutex
	cond        pthreadcond
	count       int
}

func unimplemented(name string) {
	println(name, "not implemented")
	*(*int)(unsafe.Pointer(uintptr(1231))) = 1231
}

//go:nosplit
func semacreate(mp *m) {
	if mp.initialized {
		return
	}
	mp.initialized = true
	if err := pthread_mutex_init(&mp.mutex, nil); err != 0 {
		throw("pthread_mutex_init")
	}
	// mp.mutex 必须先于 mp.cond 之前初始化
	if err := pthread_cond_init(&mp.cond, nil); err != 0 {
		throw("pthread_cond_init")
	}
}

// 休眠等待条件变量成立
// 返回0，表示条件变量目前状态符合要求
// 返回-1，表示因为超时而返回，而不是因为条件变量成立。
// 信号量的成立通过大于0的mp.count来表示。
//go:nosplit
func semasleep(ns int64) int32 {
	// 记录起始时间
	// 用于计算函数执行时间
	var start int64
	if ns >= 0 {
		start = nanotime()
	}
	mp := getg().m
	// 等待条件变量是否成立之前需先获得锁，防止数据竞争
	pthread_mutex_lock(&mp.mutex)
	for {
		if mp.count > 0 {
			// 条件变量已经成立，直接解锁返回
			mp.count--
			pthread_mutex_unlock(&mp.mutex)
			return 0
		}
		if ns >= 0 {
			spent := nanotime() - start
			if spent >= ns {
				// 执行时间已经大于或等于休眠的时间了
				// 需要返回
				pthread_mutex_unlock(&mp.mutex)
				return -1
			}
			var t timespec
			// 设置等待时间
			t.setNsec(ns - spent)
			err := pthread_cond_timedwait_relative_np(&mp.cond, &mp.mutex, &t)
			if err == _ETIMEDOUT {
				// 表示 pthread_cond_timedwait_relative_np 因超时而返回
				pthread_mutex_unlock(&mp.mutex)
				return -1
			}
		} else {
			// ns为负值，表示休眠线程永久等待
			// 在休眠之前会释放锁
			pthread_cond_wait(&mp.cond, &mp.mutex)
		}
	}
}

//go:nosplit
func semawakeup(mp *m) {
	// 改变信号量之前需要上锁
	pthread_mutex_lock(&mp.mutex)
	// 递增 mp.count 表示信号梁成立。
	mp.count++
	if mp.count > 0 {
		// 有等待线程在等待
		// 唤醒等待线程
		pthread_cond_signal(&mp.cond)
	}
	//释放锁，等待线程唤醒后需要持有锁
	pthread_mutex_unlock(&mp.mutex)
}

// The read and write file descriptors used by the sigNote functions.
var sigNoteRead, sigNoteWrite int32

// sigNoteSetup initializes an async-signal-safe note.
//
// The current implementation of notes on Darwin is not async-signal-safe,
// because the functions pthread_mutex_lock, pthread_cond_signal, and
// pthread_mutex_unlock, called by semawakeup, are not async-signal-safe.
// There is only one case where we need to wake up a note from a signal
// handler: the sigsend function. The signal handler code does not require
// all the features of notes: it does not need to do a timed wait.
// This is a separate implementation of notes, based on a pipe, that does
// not support timed waits but is async-signal-safe.
func sigNoteSetup(*note) {
	if sigNoteRead != 0 || sigNoteWrite != 0 {
		throw("duplicate sigNoteSetup")
	}
	var errno int32
	sigNoteRead, sigNoteWrite, errno = pipe()
	if errno != 0 {
		throw("pipe failed")
	}
	closeonexec(sigNoteRead)
	closeonexec(sigNoteWrite)

	// Make the write end of the pipe non-blocking, so that if the pipe
	// buffer is somehow full we will not block in the signal handler.
	// Leave the read end of the pipe blocking so that we will block
	// in sigNoteSleep.
	setNonblock(sigNoteWrite)
}

// sigNoteWakeup wakes up a thread sleeping on a note created by sigNoteSetup.
func sigNoteWakeup(*note) {
	var b byte
	write(uintptr(sigNoteWrite), unsafe.Pointer(&b), 1)
}

// sigNoteSleep waits for a note created by sigNoteSetup to be woken.
func sigNoteSleep(*note) {
	for {
		var b byte
		entersyscallblock()
		n := read(sigNoteRead, unsafe.Pointer(&b), 1)
		exitsyscall()
		if n != -_EINTR {
			return
		}
	}
}

// BSD interface for threading.
func osinit() {
	// pthread_create delayed until end of goenvs so that we
	// can look at the environment first.

	ncpu = getncpu()
	physPageSize = getPageSize()

	osinit_hack()
}

func sysctlbynameInt32(name []byte) (int32, int32) {
	out := int32(0)
	nout := unsafe.Sizeof(out)
	ret := sysctlbyname(&name[0], (*byte)(unsafe.Pointer(&out)), &nout, nil, 0)
	return ret, out
}

//go:linkname internal_cpu_getsysctlbyname internal/cpu.getsysctlbyname
func internal_cpu_getsysctlbyname(name []byte) (int32, int32) {
	return sysctlbynameInt32(name)
}

const (
	_CTL_HW      = 6
	_HW_NCPU     = 3
	_HW_PAGESIZE = 7
)

func getncpu() int32 {
	// Use sysctl to fetch hw.ncpu.
	mib := [2]uint32{_CTL_HW, _HW_NCPU}
	out := uint32(0)
	nout := unsafe.Sizeof(out)
	ret := sysctl(&mib[0], 2, (*byte)(unsafe.Pointer(&out)), &nout, nil, 0)
	if ret >= 0 && int32(out) > 0 {
		return int32(out)
	}
	return 1
}

func getPageSize() uintptr {
	// Use sysctl to fetch hw.pagesize.
	mib := [2]uint32{_CTL_HW, _HW_PAGESIZE}
	out := uint32(0)
	nout := unsafe.Sizeof(out)
	ret := sysctl(&mib[0], 2, (*byte)(unsafe.Pointer(&out)), &nout, nil, 0)
	if ret >= 0 && int32(out) > 0 {
		return uintptr(out)
	}
	return 0
}

var urandom_dev = []byte("/dev/urandom\x00")

//go:nosplit
func getRandomData(r []byte) {
	fd := open(&urandom_dev[0], 0 /* O_RDONLY */, 0)
	n := read(fd, unsafe.Pointer(&r[0]), int32(len(r)))
	closefd(fd)
	extendRandom(r, int(n))
}
// 解析环境变量，程序中通过os.Getenv 获取的环境变量是在这里初始化的（Windows除外）
func goenvs() {
	goenvs_unix()
}

// 为mp创建关联线程，没有使用CGO的情况相才会使用此函数创建线程
// May run with m.p==nil, so write barriers are not allowed.
//
//go:nowritebarrierrec
func newosproc(mp *m) {
	stk := unsafe.Pointer(mp.g0.stack.hi)
	// 至此：
	// mac 平台下，至此 mp.g0.stack.hi = mp.g0.stack.lo = 0
	// linux 平台下，mp.g0.stack.hi-mp.g0.stack.lo = 8192
	//
	if false {
		print("newosproc stk=", stk, " m=", mp, " g=", mp.g0, " id=", mp.id, " ostk=", &mp, "\n")
	}

	// Initialize an attribute object.
	var attr pthreadattr
	var err int32
	err = pthread_attr_init(&attr)
	if err != 0 {
		writeErrStr(failthreadcreate)
		exit(1)
	}

	// Find out OS stack size for our own stack guard.
	var stacksize uintptr
	if pthread_attr_getstacksize(&attr, &stacksize) != 0 {
		writeErrStr(failthreadcreate)
		exit(1)
	}
	// mac os 平台下此值 stacksize=524288
	mp.g0.stack.hi = stacksize // for mstart

	// Tell the pthread library we won't join with this thread.
	if pthread_attr_setdetachstate(&attr, _PTHREAD_CREATE_DETACHED) != 0 {
		writeErrStr(failthreadcreate)
		exit(1)
	}

	// Finally, create the thread. It starts at mstart_stub, which does some low-level
	// setup and then calls mstart.
	var oset sigset
	sigprocmask(_SIG_SETMASK, &sigset_all, &oset)
	// oset = 0
	// 新的线程克隆是会继承刚刚设置的值为 sigset_all 的 _SIG_SETMASK
	err = retryOnEAGAIN(func() int32 {
		return pthread_create(&attr, abi.FuncPCABI0(mstart_stub), unsafe.Pointer(mp))
	})
	// 线程克隆完毕后恢复之前的 _SIG_SETMASK
	sigprocmask(_SIG_SETMASK, &oset, nil)
	if err != 0 {
		writeErrStr(failthreadcreate)
		exit(1)
	}
}

// glue code to call mstart from pthread_create.
func mstart_stub()

// newosproc0 is a version of newosproc that can be called before the runtime
// is initialized.
//
// This function is not safe to use after initialization as it does not pass an M as fnarg.
//
//go:nosplit
func newosproc0(stacksize uintptr, fn uintptr) {
	// Initialize an attribute object.
	var attr pthreadattr
	var err int32
	err = pthread_attr_init(&attr)
	if err != 0 {
		writeErrStr(failthreadcreate)
		exit(1)
	}

	// The caller passes in a suggested stack size,
	// from when we allocated the stack and thread ourselves,
	// without libpthread. Now that we're using libpthread,
	// we use the OS default stack size instead of the suggestion.
	// Find out that stack size for our own stack guard.
	if pthread_attr_getstacksize(&attr, &stacksize) != 0 {
		writeErrStr(failthreadcreate)
		exit(1)
	}
	g0.stack.hi = stacksize // for mstart
	memstats.stacks_sys.add(int64(stacksize))

	// Tell the pthread library we won't join with this thread.
	if pthread_attr_setdetachstate(&attr, _PTHREAD_CREATE_DETACHED) != 0 {
		writeErrStr(failthreadcreate)
		exit(1)
	}

	// Finally, create the thread. It starts at mstart_stub, which does some low-level
	// setup and then calls mstart.
	var oset sigset
	sigprocmask(_SIG_SETMASK, &sigset_all, &oset)
	err = pthread_create(&attr, fn, nil)
	sigprocmask(_SIG_SETMASK, &oset, nil)
	if err != 0 {
		writeErrStr(failthreadcreate)
		exit(1)
	}
}

// Called to do synchronous initialization of Go code built with
// -buildmode=c-archive or -buildmode=c-shared.
// None of the Go runtime is initialized.
//
//go:nosplit
//go:nowritebarrierrec
func libpreinit() {
	// For c-archive/c-shared this is called by libpreinit with
	// preinit == true.
	initsig(true)
}

// mpreinit()
// 初始化 m.gsignal，mac和linux下大小都为 32*1024
//
// Called to initialize a new m (including the bootstrap m).
// Called on the parent thread (main thread in case of bootstrap), can allocate memory.
func mpreinit(mp *m) {
	mp.gsignal = malg(32 * 1024) // OS X wants >= 8K
	mp.gsignal.m = mp
	if GOOS == "darwin" && GOARCH == "arm64" {
		// mlock the signal stack to work around a kernel bug where it may
		// SIGILL when the signal stack is not faulted in while a signal
		// arrives. See issue 42774.
		mlock(unsafe.Pointer(mp.gsignal.stack.hi-physPageSize), physPageSize)
	}
}

// minit()
// 设置线程信号处理栈、从阻塞信号列表中清除某些不应该被阻塞的信号
//
// Called to initialize a new m (including the bootstrap m).
// Called on the new thread, cannot allocate memory.
func minit() {
	// iOS does not support alternate signal stack.
	// The signal handler handles it directly.
	if !(GOOS == "ios" && GOARCH == "arm64") {
		// 设置 alternate signal stack
		minitSignalStack()
	}

	// 如果线程的信号处理阻塞了某些信号，则清除一些不能被阻塞的信号
	minitSignalMask()
	getg().m.procid = uint64(pthread_self())
}

// Called from dropm to undo the effect of an minit.
//
//go:nosplit
func unminit() {
	// iOS does not support alternate signal stack.
	// See minit.
	if !(GOOS == "ios" && GOARCH == "arm64") {
		unminitSignals()
	}
}

// Called from exitm, but not from drop, to undo the effect of thread-owned
// resources in minit, semacreate, or elsewhere. Do not take locks after calling this.
func mdestroy(mp *m) {
}

//go:nosplit
func osyield_no_g() {
	usleep_no_g(1)
}

//go:nosplit
func osyield() {
	usleep(1)
}

const (
	_NSIG        = 32
	_SI_USER     = 0 /* empirically true, but not what headers say */
	_SIG_BLOCK   = 1
	_SIG_UNBLOCK = 2
	_SIG_SETMASK = 3
	_SS_DISABLE  = 4
)

//extern SigTabTT runtime·sigtab[];

type sigset uint32

var sigset_all = ^sigset(0)

//go:nosplit
//go:nowritebarrierrec
func setsig(i uint32, fn uintptr) {
	var sa usigactiont
	sa.sa_flags = _SA_SIGINFO | _SA_ONSTACK | _SA_RESTART
	sa.sa_mask = ^uint32(0)
	if fn == abi.FuncPCABIInternal(sighandler) { // abi.FuncPCABIInternal(sighandler) matches the callers in signal_unix.go
		if iscgo {
			fn = abi.FuncPCABI0(cgoSigtramp)
		} else {
			fn = abi.FuncPCABI0(sigtramp)
		}
	}
	*(*uintptr)(unsafe.Pointer(&sa.__sigaction_u)) = fn
	sigaction(i, &sa, nil)
}

// sigtramp is the callback from libc when a signal is received.
// It is called with the C calling convention.
func sigtramp()
func cgoSigtramp()

//go:nosplit
//go:nowritebarrierrec
func setsigstack(i uint32) {
	var osa usigactiont
	sigaction(i, nil, &osa)
	handler := *(*uintptr)(unsafe.Pointer(&osa.__sigaction_u))
	if osa.sa_flags&_SA_ONSTACK != 0 {
		return
	}
	var sa usigactiont
	*(*uintptr)(unsafe.Pointer(&sa.__sigaction_u)) = handler
	sa.sa_mask = osa.sa_mask
	sa.sa_flags = osa.sa_flags | _SA_ONSTACK
	sigaction(i, &sa, nil)
}

// getsig()
// 获取信号i对应的处理函数的第一条指令的地址
//
//go:nosplit
//go:nowritebarrierrec
func getsig(i uint32) uintptr {
	var sa usigactiont
	sigaction(i, nil, &sa)
	return *(*uintptr)(unsafe.Pointer(&sa.__sigaction_u))
}

// setSignalstackSP sets the ss_sp field of a stackt.
//
//go:nosplit
func setSignalstackSP(s *stackt, sp uintptr) {
	*(*uintptr)(unsafe.Pointer(&s.ss_sp)) = sp
}

//go:nosplit
//go:nowritebarrierrec
func sigaddset(mask *sigset, i int) {
	*mask |= 1 << (uint32(i) - 1)
}

func sigdelset(mask *sigset, i int) {
	*mask &^= 1 << (uint32(i) - 1)
}

func setProcessCPUProfiler(hz int32) {
	setProcessCPUProfilerTimer(hz)
}

func setThreadCPUProfiler(hz int32) {
	setThreadCPUProfilerHz(hz)
}

//go:nosplit
func validSIGPROF(mp *m, c *sigctxt) bool {
	return true
}

//go:linkname executablePath os.executablePath
var executablePath string

func sysargs(argc int32, argv **byte) {
	// skip over argv, envv and the first string will be the path
	n := argc + 1
	for argv_index(argv, n) != nil {
		n++
	}
	executablePath = gostringnocopy(argv_index(argv, n+1))

	// strip "executable_path=" prefix if available, it's added after OS X 10.11.
	const prefix = "executable_path="
	if len(executablePath) > len(prefix) && executablePath[:len(prefix)] == prefix {
		executablePath = executablePath[len(prefix):]
	}
}

func signalM(mp *m, sig int) {
	pthread_kill(pthread(mp.procid), uint32(sig))
}

// sigPerThreadSyscall is only used on linux, so we assign a bogus signal
// number.
const sigPerThreadSyscall = 1 << 31

//go:nosplit
func runPerThreadSyscall() {
	throw("runPerThreadSyscall only valid on linux")
}
