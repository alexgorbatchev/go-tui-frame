package frame

// deniedByExitedGroup reports whether kill(2) refused to signal process group
// pgid with EPERM only because all its members have exited or are exiting.
// Linux never does: such a member stays in its group until wait reaps it
// (release_task in kernel/exit.c) and accepts the signals the caller may send
// it. Once it is exiting, prepare_signal drops the signal under
// SIGNAL_GROUP_EXIT and __send_signal_locked still returns 0 (kernel/signal.c).
// __kill_pgrp_info succeeds once any member accepts, so Linux's EPERM always
// comes from a member that refused the caller.
func deniedByExitedGroup(int) (bool, error) { return false, nil }
