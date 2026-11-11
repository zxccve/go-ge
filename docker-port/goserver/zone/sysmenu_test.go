package zone

import (
	"bytes"
	"context"
	"log"
	"os"
	"testing"

	"geserver/protocol"
)

// The System Option menu's two buttons, measured against the reference's own answers
// (CCommander::OnSysMenu, source2\geServer\ZServerLib\Commander_Packet.cpp:734-765).
//
//	selection 0 (BARRACK)     -> ZC_MOVE_BARRACK 3597, two bytes
//	selection 2 (SAVE POINT)  -> a MOVE_ZONE_OK to the map of the saved warp id
//
// Both are driven through Handler.sysMenuSelection, which is the function last_point.go's
// sysMenu calls, so the test covers the dispatch and not only the builders.

func sysMenuHandler(t *testing.T) *Handler {
	t.Helper()
	return &Handler{
		Log: log.New(os.Stderr, "", 0),
		// A saved start point of warp 213, which is Coimbra (15)'s restart row, and the
		// player is standing on map 10 - so the move has a destination that is not here.
		ZonePoints: &ZonePoints{
			Maps: map[string]MapEntry{
				"15": {Name: "coim_basic", Spawn: &MapPoint{X: 100, Y: 200, Cell: cellPtr(7)}},
				"10": {Name: "reb_basic", Restart: &RestartPoint{WarpID: 213}},
			},
		},
		MapID: 10,
	}
}

func cellPtr(v int32) *int32 { return &v }

// TestSysMenuBarrackSendsLogoutOkThenMoveBarrack proves selection 0 answers TWO packets in
// the client's own order: ZC_LOGOUT_OK first (it sets m_isAbleToLogout, without which the
// client ignores the switch target - src\zone_server.py's build_logout_ok comment), then the
// two-byte ZC_MOVE_BARRACK.
func TestSysMenuBarrackSendsMoveBarrack(t *testing.T) {
	h := sysMenuHandler(t)
	sess := &Session{AccountID: 35}

	out, err := h.sysMenuSelection(context.Background(), sess, protocol.Frame{
		Cmd: CmdCZSysMenu, Data: sysMenuPacket(sysMenuToBarrack),
	})
	if err != nil {
		t.Fatalf("sysMenuSelection: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("%d packet(s) answered, want 2 (ZC_LOGOUT_OK then ZC_MOVE_BARRACK)", len(out))
	}
	wantLogout := []byte{byte(CmdZCLogoutOK & 0xFF), byte(CmdZCLogoutOK >> 8)}
	if !bytes.Equal(out[0], wantLogout) {
		t.Fatalf("first answer = % x, want ZC_LOGOUT_OK % x", out[0], wantLogout)
	}
	wantMove := []byte{byte(CmdZCMoveBarrack & 0xFF), byte(CmdZCMoveBarrack >> 8)}
	if !bytes.Equal(out[1], wantMove) {
		t.Fatalf("second answer = % x, want ZC_MOVE_BARRACK % x", out[1], wantMove)
	}
	if !sess.CloseAfterAnswer {
		t.Fatalf("the session is not marked to close after the answer")
	}
}

// TestSavePointOnThisMapMovesNobody proves the reference's same-zone refusal: when the saved
// warp names the map the player already stands on, nothing is sent at all.
func TestSavePointOnThisMapMovesNobody(t *testing.T) {
	h := sysMenuHandler(t)
	h.SaveWarpIDOf = func(ctx context.Context, commanderID int32) (int, bool, error) {
		return 213, true, nil
	}
	h.MapID = 15 // the save point's own map
	sess := &Session{AccountID: 35, CommanderID: 24}

	out, err := h.sysMenuSelection(context.Background(), sess, protocol.Frame{
		Cmd: CmdCZSysMenu, Data: sysMenuPacket(sysMenuToSavePoint),
	})
	if err != nil {
		t.Fatalf("sysMenuSelection: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("%d packet(s) answered for a same-map save point, want 0", len(out))
	}
}

// TestSavePointWithoutASavedWarpSaysNothing proves the ordinary "never pressed Save" state:
// commander.point is 0, the reader answers found=false, and the reply is silence - not an
// error and not a move to an invented place.
func TestSavePointWithoutASavedWarpSaysNothing(t *testing.T) {
	h := sysMenuHandler(t)
	h.SaveWarpIDOf = func(ctx context.Context, commanderID int32) (int, bool, error) {
		return 0, false, nil
	}
	sess := &Session{AccountID: 35, CommanderID: 24}

	out, err := h.sysMenuSelection(context.Background(), sess, protocol.Frame{
		Cmd: CmdCZSysMenu, Data: sysMenuPacket(sysMenuToSavePoint),
	})
	if err != nil {
		t.Fatalf("sysMenuSelection: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("%d packet(s) answered with no saved warp, want 0", len(out))
	}
}

// sysMenuPacket builds the CZ_SYSMENU body: PKS_SECURITY_DEFAULT's 10 bytes then the
// selection byte (the client's own registered size, 12).
func sysMenuPacket(selection uint8) []byte {
	body := make([]byte, sysMenuSize)
	body[10] = selection
	return body
}
