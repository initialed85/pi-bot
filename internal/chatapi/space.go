// Package chatapi: see client.go for the credential model. This file covers the one read the room
// listener needs that its own payload does not provide: what kind of space a message landed in.
package chatapi

import (
	"context"
	"fmt"
	"strings"
)

// SpaceInfo is the part of a Chat space the bridge routes on.
//
// A Workspace Events message does not reliably say whether its space is a 1:1 or a room: the
// subscription's fieldMask decides what arrives, and validateOnly does not validate a mask, so the
// only honest answer is "ask Chat". The app credential can read any space the app is in, which is
// exactly the set of spaces the bridge replies in.
type SpaceInfo struct {
	Name      string
	SpaceType string // "ROOM", "DM" or "DIRECT_MESSAGE" as Chat reports it
	// SingleUserBotDM is Chat's own flag for a 1:1 between one human and one bot, which is the case
	// the bridge always answers inline.
	SingleUserBotDM bool
}

// DM reports the routing decision the bridge cares about, trusting Chat's bot-DM flag over the
// space type: a two-human space reports DIRECT_MESSAGE while behaving like a room, and the bridge
// has already been bitten by trusting spaceType once (see FINDINGS).
func (s *SpaceInfo) DM() bool {
	if s == nil {
		return false
	}
	if s.SingleUserBotDM {
		return true
	}
	return strings.EqualFold(s.SpaceType, "DM")
}

// GetSpace reads one space. It is meant to be called once per space and cached by the caller.
func (c *Client) GetSpace(ctx context.Context, name string) (*SpaceInfo, error) {
	if name == "" {
		return nil, fmt.Errorf("chatapi: get space: empty name")
	}
	sp, err := c.svc.Spaces.Get(name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("chatapi: get space %s: %w", name, err)
	}
	return &SpaceInfo{
		Name:            firstOf(sp.Name, name),
		SpaceType:       sp.SpaceType,
		SingleUserBotDM: sp.SingleUserBotDm,
	}, nil
}
