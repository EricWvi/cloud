package taskrun

// SetStopRead makes Stop call read once it has read the run as its caller saw it, before it waits
// for the task's lock, so a test can order two concurrent stops.
func SetStopRead(h *Host, read func()) { h.stopRead = read }
