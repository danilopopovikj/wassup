package model

// Normalized condition kinds. Probes translate their source's vocabulary
// into these; the state engine and the labels only know these.
const (
	CondCrashLoopBackOff   = "CrashLoopBackOff"
	CondOOMKilled          = "OOMKilled"
	CondImagePullBackOff   = "ImagePullBackOff"
	CondEvicted            = "Evicted"
	CondNotReady           = "NotReady"
	CondMemoryPressure     = "MemoryPressure"
	CondDiskPressure       = "DiskPressure"
	CondRebooted           = "Rebooted"
	CondTargetUnhealthy    = "TargetUnhealthy"
	CondHealthCheckFailing = "HealthCheckFailing"
	CondCertExpired        = "CertExpired"
	CondCertExpiring       = "CertExpiring"
	CondCertRenewalFailed  = "CertRenewalFailed"
	CondReplicationBroken  = "ReplicationBroken"
	CondSlotInactive       = "SlotInactive"
	CondDiskFull           = "DiskFull"
	CondNoData             = "NoData"
	CondTimeout            = "Timeout"
	CondJobFailed          = "JobFailed"
	CondJobRunning         = "JobRunning"
	CondTaskRunning        = "TaskRunning"
	CondCacheFull          = "CacheFull"
	CondFirewallDenied     = "FirewallDenied"
	CondConnectionRefused  = "ConnectionRefused"
	CondPoolExhausted      = "PoolExhausted"
	CondMigration          = "Migration"
	CondBackup             = "Backup"
	CondVacuum             = "Vacuum"
	CondSwitchover         = "Switchover"
	CondScaled             = "Scaled"
)

// HasCondition reports whether kind is present.
func HasCondition(conds []Condition, kind string) (Condition, bool) {
	for _, c := range conds {
		if c.Kind == kind {
			return c, true
		}
	}
	return Condition{}, false
}
