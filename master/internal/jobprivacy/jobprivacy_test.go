package jobprivacy

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/jobv1"
)

// The audiences a job can have for one kind.
var (
	audNull = []string(nil)
	audNone = []string{}
	audAll  = []string{AudienceAuthenticated}
)

// audiences are the audiences of the tests, by name.
var audiences = map[string][]string{"NULL": audNull, "{}": audNone, "{authenticated}": audAll}

// switchSet is an owner's preference row, or none.
type switchSet struct {
	name           string
	titles, logs   bool
	hasPreferences bool
}

// switchSets are the owner preferences of the tests. Without a row both switches are on.
var switchSets = []switchSet{
	{name: "no row", titles: true, logs: true},
	{name: "both on", titles: true, logs: true, hasPreferences: true},
	{name: "titles off", titles: false, logs: true, hasPreferences: true},
	{name: "logs off", titles: true, logs: false, hasPreferences: true},
	{name: "both off", titles: false, logs: false, hasPreferences: true},
}

// policies are every combination of allowed and forbidden kinds. A forbidden kind stands for a
// missing row as well, which ReadPolicy turns into the same value (TestPolicyMissingRowForbids).
func policies() []Policy {
	var out []Policy
	for _, t := range []bool{true, false} {
		for _, l := range []bool{true, false} {
			for _, c := range []bool{true, false} {
				out = append(out, Policy{
					Title:  KindPolicy{Allowed: t, NewJobAudience: audAll},
					Logs:   KindPolicy{Allowed: l, NewJobAudience: audAll},
					Config: KindPolicy{Allowed: c, NewJobAudience: audAll},
				})
			}
		}
	}
	return out
}

// isOpen reports whether the audience includes all authenticated users.
func isOpen(aud []string) bool { return aud == nil || len(aud) > 0 }

func TestEffective(t *testing.T) {
	owner := model.UserID(1)
	const other = model.UserID(2)
	for _, p := range policies() {
		for _, sw := range switchSets {
			for tn, ta := range audiences {
				for ln, la := range audiences {
					for cn, ca := range audiences {
						for _, ownerless := range []bool{false, true} {
							for _, exists := range []bool{true, false} {
								ownerID := &owner
								if ownerless {
									ownerID = nil
								}
								j := JobFacts{
									JobID: "j", OwnerID: ownerID, Exists: exists,
									Title: ta, Logs: la, Config: ca,
									ShareTitles: sw.titles, ShareLogs: sw.logs,
								}
								name := fmt.Sprintf("%+v/%s/%s,%s,%s/ownerless=%v/exists=%v",
									p, sw.name, tn, ln, cn, ownerless, exists)

								// Admins see everything, also a job without a jobs row.
								require.Equal(t, AllCaps, Effective(Viewer{ID: 9, Admin: true}, j, p), name)

								got := Effective(Viewer{ID: other}, j, p)
								if !exists {
									require.Equal(t, Caps{}, got, name)
									require.Equal(t, Caps{}, Effective(Viewer{ID: owner}, j, p), name)
									continue
								}
								if !ownerless {
									require.Equal(t, AllCaps, Effective(Viewer{ID: owner}, j, p), name)
								} else {
									require.Equal(t, got, Effective(Viewer{ID: owner}, j, p), name)
								}

								wantTitle := p.Title.Allowed && sw.titles && isOpen(ta)
								want := Caps{
									ViewTitle:  wantTitle,
									ViewLogs:   wantTitle && p.Logs.Allowed && sw.logs && isOpen(la),
									ViewConfig: wantTitle && p.Config.Allowed && isOpen(ca),
								}
								require.Equal(t, want, got, name)
								require.False(t, got.ManageSharing, name)
								// Logs and config need the title.
								require.True(t, !got.ViewLogs || got.ViewTitle, name)
								require.True(t, !got.ViewConfig || got.ViewTitle, name)
							}
						}
					}
				}
			}
		}
	}
}

func TestEffectiveDefaults(t *testing.T) {
	owner := model.UserID(1)
	public := Policy{
		Title:  KindPolicy{Allowed: true, NewJobAudience: audAll},
		Logs:   KindPolicy{Allowed: true, NewJobAudience: audAll},
		Config: KindPolicy{Allowed: true, NewJobAudience: audAll},
	}
	other := Viewer{ID: 2}

	// A job created before job privacy (NULL audiences) of an owner without a row is public.
	legacy := JobFacts{OwnerID: &owner, Exists: true, ShareTitles: true, ShareLogs: true}
	require.Equal(t, Caps{ViewTitle: true, ViewLogs: true, ViewConfig: true},
		Effective(other, legacy, public))

	// A missing policy row is forbidden: ReadPolicy gives Allowed false.
	forbidden := public
	forbidden.Logs = KindPolicy{NewJobAudience: audNone}
	require.Equal(t, Caps{ViewTitle: true, ViewConfig: true}, Effective(other, legacy, forbidden))

	// A hidden title hides the logs and the config even when they are shared.
	hiddenTitle := legacy
	hiddenTitle.Title = audNone
	hiddenTitle.Logs, hiddenTitle.Config = audAll, audAll
	require.Equal(t, Caps{}, Effective(other, hiddenTitle, public))

	// A config can be tighter than the title.
	hiddenConfig := legacy
	hiddenConfig.Config = audNone
	require.Equal(t, Caps{ViewTitle: true, ViewLogs: true}, Effective(other, hiddenConfig, public))
}

func TestPolicyFromRows(t *testing.T) {
	// A read error is Unavailable and never the default.
	p, err := policyFromRows(nil, errors.New("connection refused"))
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Equal(t, Policy{}, p)

	// The seeded rows.
	p, err = policyFromRows([]policyRow{
		{Kind: "title", SharingAllowed: true, NewJobAudience: audAll},
		{Kind: "logs", SharingAllowed: false, NewJobAudience: audNone},
		{Kind: "config", SharingAllowed: true, NewJobAudience: audNone},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, Policy{
		Title:  KindPolicy{Allowed: true, NewJobAudience: audAll},
		Logs:   KindPolicy{Allowed: false, NewJobAudience: audNone},
		Config: KindPolicy{Allowed: true, NewJobAudience: audNone},
	}, p)

	// A missing row forbids its kind, with the audience {}.
	p, err = policyFromRows([]policyRow{
		{Kind: "title", SharingAllowed: true, NewJobAudience: audAll},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, KindPolicy{Allowed: false, NewJobAudience: audNone}, p.Logs)
	require.Equal(t, KindPolicy{Allowed: false, NewJobAudience: audNone}, p.Config)
	require.NotNil(t, p.Logs.NewJobAudience)
}

func TestDeniedErrors(t *testing.T) {
	for k, msg := range map[Kind]string{Logs: "logs not shared", Config: "config not shared"} {
		err := deniedError(k)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
		require.Equal(t, msg, status.Convert(err).Message())
	}
	// The title is redacted, never refused.
	require.Equal(t, codes.Internal, status.Code(deniedError(Title)))
}

func TestKinds(t *testing.T) {
	for _, k := range Kinds {
		got, ok := KindFromProto(k.Proto())
		require.True(t, ok)
		require.Equal(t, k, got)
	}
	_, ok := KindFromProto(jobv1.JobContentKind_JOB_CONTENT_KIND_UNSPECIFIED)
	require.False(t, ok)
	_, ok = KindFromProto(jobv1.JobContentKind(42))
	require.False(t, ok)
	require.Equal(t, []string{"title", "logs", "config"},
		[]string{Title.String(), Logs.String(), Config.String()})
}

func TestCapsProto(t *testing.T) {
	c := Caps{ViewTitle: true, ViewConfig: true}
	require.True(t, proto.Equal(&jobv1.JobCapabilities{ViewTitle: true, ViewConfig: true}, c.Proto()))
	require.True(t, c.Allows(Title))
	require.False(t, c.Allows(Logs))
	require.True(t, c.Allows(Config))
}

func TestAudienceConversions(t *testing.T) {
	require.Equal(t, audAll, Audience(true))
	require.Equal(t, audNone, Audience(false))
	require.Nil(t, AudienceProto(audNull))
	require.True(t, proto.Equal(&jobv1.JobAudience{}, AudienceProto(audNone)))
	require.True(t, proto.Equal(&jobv1.JobAudience{AllUsers: true}, AudienceProto(audAll)))
}

func TestPolicyRules(t *testing.T) {
	// A missing row is forbidden with an audience of nobody else.
	p, err := policyFromRows([]policyRow{
		{Kind: "config", SharingAllowed: true, NewJobAudience: audAll},
		{Kind: "title", SharingAllowed: true, NewJobAudience: audNone},
	}, nil)
	require.NoError(t, err)
	got := p.Rules()
	want := []*jobv1.JobPrivacyRule{
		{
			Kind: jobv1.JobContentKind_JOB_CONTENT_KIND_TITLE, SharingAllowed: true,
			NewJobAudience: &jobv1.JobAudience{},
		},
		{Kind: jobv1.JobContentKind_JOB_CONTENT_KIND_LOGS, NewJobAudience: &jobv1.JobAudience{}},
		{
			Kind: jobv1.JobContentKind_JOB_CONTENT_KIND_CONFIG, SharingAllowed: true,
			NewJobAudience: &jobv1.JobAudience{AllUsers: true},
		},
	}
	require.Len(t, got, len(want))
	for i := range want {
		require.True(t, proto.Equal(want[i], got[i]), "rule %d: %v", i, got[i])
	}
}

func TestRequireAllowed(t *testing.T) {
	yes, no := true, false
	p := Policy{Title: KindPolicy{Allowed: true}, Logs: KindPolicy{}, Config: KindPolicy{}}
	require.NoError(t, requireAllowed(p, map[Kind]*bool{Title: &yes, Logs: &no, Config: nil}))
	err := requireAllowed(p, map[Kind]*bool{Title: &yes, Logs: &yes, Config: &yes})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Equal(t, "sharing of logs is forbidden", status.Convert(err).Message())
}
