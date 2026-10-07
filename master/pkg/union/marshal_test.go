package union

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type unionA struct {
	A int `json:"a"`
}

type unionWithOptional struct {
	A        *unionA `union:"type,a" json:"-"`
	Plain    int     `json:"plain"`
	Optional *bool   `json:"optional,omitempty"`
}

type unionWithAdvancedTag struct {
	A     *unionA `union:"type,a" json:"-"`
	Plain int     `json:"plain,string"`
}

func TestMarshalOmitsNilPointersWithOmitempty(t *testing.T) {
	raw, err := Marshal(unionWithOptional{A: &unionA{A: 1}, Plain: 2})
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"a","a":1,"plain":2}`, string(raw))

	no := false
	raw, err = Marshal(unionWithOptional{A: &unionA{A: 1}, Plain: 2, Optional: &no})
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"a","a":1,"plain":2,"optional":false}`, string(raw))

	_, err = Marshal(unionWithAdvancedTag{A: &unionA{}})
	require.Error(t, err)
}
