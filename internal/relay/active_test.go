package relay

import (
	"slices"
	"testing"
)

func TestActiveReservationsTrackIDs(t *testing.T) {
	var active ActiveReservations
	active.Register(3)
	active.Register(1)
	active.Register(3)
	ids := active.IDs()
	slices.Sort(ids)
	if !slices.Equal(ids, []int64{1, 3}) {
		t.Fatalf("active IDs=%v", ids)
	}
	active.Remove(1)
	if ids := active.IDs(); !slices.Equal(ids, []int64{3}) {
		t.Fatalf("active IDs after removal=%v", ids)
	}
}

func TestBeginDrainingRejectsNewRequests(t *testing.T) {
	service, _, key := serviceFixture(t, &fakeClient{})
	service.BeginDraining()
	_, _, err := service.Begin(t.Context(), Request{Token: key.Token, Model: "public-model"})
	if err != ErrSettlementUnavailable {
		t.Fatalf("err=%v, want settlement unavailable", err)
	}
}
