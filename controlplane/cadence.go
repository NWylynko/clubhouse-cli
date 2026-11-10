package controlplane

import "time"

const (
	MemberCycle = 5 * time.Second

	MemberOnlineAfter = 20 * time.Second

	SnapshotEvery = 12

	SSEPingInterval = 15 * time.Second

	StreamSilentAfter = 35 * time.Second

	MirrorFloor = 60 * time.Second

	RemoteConductorFloor = 10 * time.Second

	LogsFloor = 60 * time.Second

	IdleTTL = 3 * time.Hour

	DoorRefuseDelay = 2 * time.Second

	RedeemPendingPerSession   = 64
	RedeemConcurrentPerSource = 16
	RedeemAttemptsPerMinute   = 60
	RedeemAttemptBurst        = 40

	StreamPerMemberMax = 3
	StreamPerPinMax    = RosterLimit * StreamPerMemberMax

	HeldConnectionGlobalMax = BudgetDefaultHeldConnections

	KnockNameMax      = 64
	KnockMachineMax   = 64
	KnockRequestIDMax = 64

	EndpointURLMax     = 512
	StartFailReasonMax = 400
	MaxBranchesAhead   = 32
	MaxBranchNameLen   = 255
	MemberNameMax      = 64
	MemberMachineMax   = 64
	RoleActivityMax    = 200
	RoleModelMax       = 200

	CreateAttemptsPerHour = 30
	CreateAttemptBurst    = 30

	RedeemUnknownPINDistinctMax = 20
	RedeemUnknownPINWindow      = 5 * time.Minute

	BoxPerSourcePerDay    = 10
	BoxPerSessionLifetime = 3
	BoxConcurrentGlobal   = BudgetDefaultBoxConcurrent
	BoxHoldStaleAfter     = 25 * time.Minute

	EventsPerMemberPerMinute = 60
	EventsPerMemberBurst     = 60

	EventIntentMax = 512
	EventSHAMax    = 64
)

func Floor(healthy time.Duration, streamLive bool) time.Duration {
	if streamLive {
		return healthy
	}
	return MemberCycle
}
