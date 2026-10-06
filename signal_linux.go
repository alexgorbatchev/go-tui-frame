package frame

// deniedByExitedGroup reports whether kill(2) refused to signal process group
// pgid with EPERM only because all its members have exited or are exiting.
// Linux never does. Such a member stays in its group, accepting the signals
// the caller may send it, until release_task (kernel/exit.c) removes it: wait
// calls it, and so does exit itself when the parent ignores SIGCHLD or sets
// SA_NOCLDWAIT, or the member was cloned with CLONE_AUTOREAP. Once the member
// is exiting, __send_signal_locked returns 0 (kernel/signal.c): prepare_signal
// drops the signal under SIGNAL_GROUP_EXIT, or queues SIGKILL during a core
// dump. __kill_pgrp_info succeeds once any member accepts, so Linux's EPERM
// always comes from a member that refused the caller.
func deniedByExitedGroup(int) (bool, error) { return false, nil }
