package frame

// deniedByExitedGroup reports whether kill(2) refused to signal process group
// pgid with EPERM only because all its members have exited. Linux never does:
// an exited member stays in its group, and accepts the signals the caller may
// send it, until wait reaps it (release_task in kernel/exit.c), and
// __kill_pgrp_info (kernel/signal.c) succeeds once any member accepts. Its
// EPERM always comes from a member that refused the caller.
func deniedByExitedGroup(int) (bool, error) { return false, nil }
