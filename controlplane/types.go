package controlplane

import (
	"errors"
	"time"
)

const (
	EndpointGit    = "git"
	EndpointLogs   = "logs"
	EndpointDev    = "dev"
	EndpointDemo   = "demo"
	EndpointPublic = "public"
	EndpointMirror = "mirror"
)

const (
	RoleHost   = "host"
	RoleClient = "client"
	RoleBox    = "box"
)

const (
	StatusLive     = "live"
	StatusDegraded = "degraded"
	StatusEnded    = "ended"
)

const (
	AccessOpen   = "open"
	AccessClosed = "closed"
	AccessFull   = "full"
)

const RosterLimit = 10

const MemberHeader = "X-Club-Member"

type Endpoint struct {
	URL       string     `json:"url"`
	Host      string     `json:"host,omitempty"`
	Port      int        `json:"port,omitempty"`
	Source    string     `json:"source,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
	Direct    string     `json:"direct,omitempty"`
}

type MemberCapability struct {
	Runtime string `json:"runtime,omitempty"`
	Version string `json:"version,omitempty"`
	PkgMgr  string `json:"pkgMgr,omitempty"`
	Score   int    `json:"score,omitempty"`
}

type Member struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Role             string            `json:"role"`
	Branch           string            `json:"branch,omitempty"`
	Machine          string            `json:"machine,omitempty"`
	HeadSHA          string            `json:"headSha,omitempty"`
	MainMirrorSHA    string            `json:"mainMirrorSha,omitempty"`
	MainMirrorHeight int               `json:"mainMirrorHeight,omitempty"`
	Harness          string            `json:"harness,omitempty"`
	Online           bool              `json:"online"`
	LastSeenAt       time.Time         `json:"lastSeenAt"`
	JoinedAt         time.Time         `json:"joinedAt,omitempty"`
	Capability       *MemberCapability `json:"capability,omitempty"`
	State            string            `json:"state,omitempty"`
	Version          string            `json:"version,omitempty"`
	Secret           string            `json:"secret,omitempty"`
}

const (
	MemberPending  = "pending"
	MemberAdmitted = "admitted"
	MemberLeft     = "left"
)

const (
	RoleIdle    = "idle"
	RoleWorking = "working"
)

const RoleStaleAfter = 15 * time.Second

type RoleAgent struct {
	Harness    string    `json:"harness,omitempty"`
	Model      string    `json:"model,omitempty"`
	Mechanical bool      `json:"mechanical,omitempty"`
	State      string    `json:"state,omitempty"`
	Activity   string    `json:"activity,omitempty"`
	Since      time.Time `json:"since,omitzero"`
	UpdatedAt  time.Time `json:"updatedAt,omitzero"`
}

func (r RoleAgent) Stale(now time.Time) bool {
	return r.UpdatedAt.IsZero() || now.Sub(r.UpdatedAt) > RoleStaleAfter
}

func (r RoleAgent) Working(now time.Time) bool {
	return r.State == RoleWorking && !r.Stale(now)
}

type ConductorRecord struct {
	Elector   string               `json:"elector,omitempty"`
	Harness   string               `json:"harness,omitempty"`
	Model     string               `json:"model,omitempty"`
	RunnerID  string               `json:"runnerId,omitempty"`
	Roles     map[string]RoleAgent `json:"roles,omitempty"`
	Active    bool                 `json:"active"`
	ElectedAt *time.Time           `json:"electedAt,omitempty"`
}

type Convergence struct {
	MainSHA              string     `json:"mainSha,omitempty"`
	BranchesAhead        []string   `json:"branchesAhead,omitempty"`
	BranchesAheadOmitted int        `json:"branchesAheadOmitted,omitempty"`
	LastMergeAt          *time.Time `json:"lastMergeAt,omitempty"`
	LastMergeBranch      string     `json:"lastMergeBranch,omitempty"`
	Watcher              string     `json:"watcher,omitempty"`
	LastError            *string    `json:"lastError"`
	UpdatedAt            *time.Time `json:"updatedAt,omitempty"`
}

const (
	BoxRequested    = "requested"
	BoxProvisioning = "provisioning"
	BoxReady        = "ready"
	BoxFailed       = "failed"
)

const (
	BoxProviderBYO        = "byo"
	BoxProviderDocker     = "docker"
	BoxProviderCloudflare = "cloudflare"
)

type BoxFacts struct {
	Target      string     `json:"target,omitempty"`
	Container   string     `json:"container,omitempty"`
	Image       string     `json:"image,omitempty"`
	Version     string     `json:"version,omitempty"`
	State       string     `json:"state,omitempty"`
	Provider    string     `json:"provider,omitempty"`
	GitURL      string     `json:"gitUrl,omitempty"`
	DevURL      string     `json:"devUrl,omitempty"`
	LogsURL     string     `json:"logsUrl,omitempty"`
	Error       string     `json:"error,omitempty"`
	RequestedAt *time.Time `json:"requestedAt,omitempty"`
	ReadyAt     *time.Time `json:"readyAt,omitempty"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
}

func (b BoxFacts) Pending() bool {
	return b.State == BoxRequested || b.State == BoxProvisioning
}

type BoxBootFailure struct {
	Reason string `json:"reason"`
}

type BoxRequest struct {
	PIN          string `json:"pin,omitempty"`
	Brief        string `json:"brief,omitempty"`
	SeedURL      string `json:"seedUrl,omitempty"`
	Image        string `json:"image,omitempty"`
	Replace      bool   `json:"replace,omitempty"`
	MemberID     string `json:"-"`
	MemberSecret string `json:"-"`
}

type StackInfo struct {
	Runtime string `json:"runtime,omitempty"`
	Version string `json:"version,omitempty"`
	PkgMgr  string `json:"pkgMgr,omitempty"`
}

const (
	ServiceGit       = "git"
	ServiceDev       = "dev"
	ServiceConductor = "conductor"
)

var Services = []string{ServiceGit, ServiceDev, ServiceConductor}

type Lease struct {
	Service      string        `json:"service"`
	Owner        string        `json:"owner,omitempty"`
	OwnerName    string        `json:"ownerName,omitempty"`
	Machine      string        `json:"machine,omitempty"`
	Generation   int           `json:"generation"`
	ExpiresAt    time.Time     `json:"expiresAt"`
	UpdatedAt    time.Time     `json:"updatedAt,omitempty"`
	StartFailure *StartFailure `json:"startFailure,omitempty"`
}

type StartFailure struct {
	MemberID string    `json:"memberId,omitempty"`
	Name     string    `json:"name,omitempty"`
	Machine  string    `json:"machine,omitempty"`
	Reason   string    `json:"reason"`
	At       time.Time `json:"at,omitzero"`
}

func (f StartFailure) Line(service string) string {
	who := f.Name
	if who == "" {
		who = f.MemberID
	}
	if f.Machine != "" {
		who += "@" + f.Machine
	}
	return service + ": " + who + " can't start it — " + f.Reason
}

func (l Lease) Live(now time.Time) bool {
	return l.Owner != "" && now.Before(l.ExpiresAt)
}

type LeaseRequest struct {
	Service         string `json:"service"`
	MemberID        string `json:"memberId"`
	Name            string `json:"name,omitempty"`
	Machine         string `json:"machine,omitempty"`
	TTLSeconds      int    `json:"ttlSeconds,omitempty"`
	To              string `json:"to,omitempty"`
	StartFailReason string `json:"startFailReason,omitempty"`
	Force           bool   `json:"force,omitempty"`
}

const DefaultLeaseTTL = 30

type Session struct {
	PIN          string              `json:"pin"`
	UID          string              `json:"uid,omitempty"`
	Generation   int                 `json:"generation"`
	Status       string              `json:"status"`
	OverlayAddr  string              `json:"overlayAddr,omitempty"`
	HostMachine  string              `json:"hostMachine,omitempty"`
	Endpoints    map[string]Endpoint `json:"endpoints"`
	Members      []Member            `json:"members"`
	Conductor    *ConductorRecord    `json:"conductor,omitempty"`
	Convergence  *Convergence        `json:"convergence,omitempty"`
	Box          *BoxFacts           `json:"box,omitempty"`
	Leases       map[string]Lease    `json:"leases,omitempty"`
	Stack        *StackInfo          `json:"stack,omitempty"`
	Harness      string              `json:"harness,omitempty"`
	SeedURL      string              `json:"seedUrl,omitempty"`
	SessionKey   string              `json:"sessionKey,omitempty"`
	RelayAddr    string              `json:"relayAddr,omitempty"`
	TelemetryURL string              `json:"telemetryUrl,omitempty"`
	Access       string              `json:"access,omitempty"`
	CreatedAt    time.Time           `json:"createdAt"`
	UpdatedAt    time.Time           `json:"updatedAt"`
	ExpiresAt    time.Time           `json:"expiresAt"`
}

const (
	EventSyncPushed   = "sync.pushed"
	EventMergeApplied = "merge.applied"
)

const EventPlacementFailed = "placement.failed"

const (
	EventBoxRequested  = "box.requested"
	EventBoxFailed     = "box.failed"
	EventBoxBootFailed = "box.boot-failed"
)

var PublishableEvents = map[string]bool{
	EventSyncPushed:   true,
	EventMergeApplied: true,
}

type EventPost struct {
	Kind    string         `json:"kind"`
	Payload map[string]any `json:"payload,omitempty"`
}

type Event struct {
	Seq       int64          `json:"seq"`
	Kind      string         `json:"kind"`
	Payload   map[string]any `json:"payload,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
}

type ClaimRequest struct {
	PIN             string     `json:"pin"`
	OverlayAddr     string     `json:"overlayAddr,omitempty"`
	HostMachine     string     `json:"hostMachine,omitempty"`
	Stack           *StackInfo `json:"stack,omitempty"`
	Harness         string     `json:"harness,omitempty"`
	SeedURL         string     `json:"seedUrl,omitempty"`
	LocalGeneration int        `json:"localGeneration,omitempty"`
	Takeover        bool       `json:"takeover,omitempty"`
	MemberName      string     `json:"memberName,omitempty"`
	MemberRole      string     `json:"memberRole,omitempty"`
}

type ClaimResponse struct {
	Session      Session           `json:"session"`
	Created      bool              `json:"created"`
	Resumed      bool              `json:"resumed"`
	JoinOnly     bool              `json:"joinOnly"`
	Takeover     bool              `json:"takeover"`
	Message      string            `json:"message,omitempty"`
	MemberID     string            `json:"memberId,omitempty"`
	Secret       string            `json:"secret,omitempty"`
	RelayTickets map[string]string `json:"relayTickets,omitempty"`
}

type CutoverRequest struct {
	NewGitURL   string `json:"newGitUrl"`
	From        string `json:"from,omitempty"`
	Drain       bool   `json:"drain,omitempty"`
	Generation  int    `json:"generation"`
	LogsURL     string `json:"logsUrl,omitempty"`
	HostMachine string `json:"hostMachine,omitempty"`
}

type MemberJoinRequest struct {
	Name       string            `json:"name"`
	Role       string            `json:"role"`
	Branch     string            `json:"branch,omitempty"`
	Machine    string            `json:"machine,omitempty"`
	Harness    string            `json:"harness,omitempty"`
	Capability *MemberCapability `json:"capability,omitempty"`
}

type RedeemRequest struct {
	Name      string `json:"name"`
	Machine   string `json:"machine,omitempty"`
	RequestID string `json:"requestId"`
	Source    string `json:"-"`
	Version   string `json:"-"`
}

const (
	DecisionAccept  = "accept"
	DecisionDecline = "decline"
)

type MemberDecision struct {
	Decision string `json:"decision"`
}

type AccessPut struct {
	Access string `json:"access"`
}

type RedeemResult struct {
	Decision     string            `json:"decision"`
	MemberID     string            `json:"memberId,omitempty"`
	Secret       string            `json:"secret,omitempty"`
	Message      string            `json:"message,omitempty"`
	Session      Session           `json:"session,omitempty"`
	RequestID    string            `json:"requestId,omitempty"`
	RelayTickets map[string]string `json:"relayTickets,omitempty"`
}

type RedeemPending struct {
	MemberID  string   `json:"memberId"`
	RequestID string   `json:"requestId"`
	Acceptors []string `json:"acceptors,omitempty"`
}

type MemberUpdate struct {
	Branch           string            `json:"branch,omitempty"`
	HeadSHA          string            `json:"headSha,omitempty"`
	MainMirrorSHA    string            `json:"mainMirrorSha,omitempty"`
	MainMirrorHeight int               `json:"mainMirrorHeight,omitempty"`
	Harness          string            `json:"harness,omitempty"`
	Capability       *MemberCapability `json:"capability,omitempty"`
	Role             string            `json:"role,omitempty"`
	Version          string            `json:"-"`
}

type MemberSync struct {
	Update       MemberUpdate           `json:"update"`
	Generation   int                    `json:"generation"`
	Hold         []string               `json:"hold,omitempty"`
	Endpoints    map[string]EndpointPut `json:"endpoints,omitempty"`
	Convergence  *Convergence           `json:"convergence,omitempty"`
	Conductor    *ConductorRecord       `json:"conductor,omitempty"`
	WantSnapshot bool                   `json:"wantSnapshot,omitempty"`
}

type MemberSyncResult struct {
	Generation   int               `json:"generation"`
	Conflict     bool              `json:"conflict,omitempty"`
	Lost         []string          `json:"lost,omitempty"`
	Session      *Session          `json:"session,omitempty"`
	RelayTickets map[string]string `json:"relayTickets,omitempty"`
	Refused      []string          `json:"refused,omitempty"`
}

type EndpointPut struct {
	URL        string `json:"url"`
	Host       string `json:"host,omitempty"`
	Port       int    `json:"port,omitempty"`
	Source     string `json:"source,omitempty"`
	Direct     string `json:"direct,omitempty"`
	Generation int    `json:"generation"`
}

type GenerationConflict struct {
	Error             string `json:"error"`
	CurrentGeneration int    `json:"currentGeneration"`
	DemoteToClient    bool   `json:"demoteToClient"`
}

type Health struct {
	OK      bool   `json:"ok"`
	Version string `json:"version,omitempty"`
	DB      string `json:"db"`
}

var ErrLeaseHeld = errors.New("lease held")
