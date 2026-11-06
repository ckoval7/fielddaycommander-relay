// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package sender

import (
	"context"
	"testing"
	"time"

	"github.com/ckoval7/fielddaycommander-relay/internal/api"
	"github.com/ckoval7/fielddaycommander-relay/internal/contact"
)

func TestTheChoiceFillsABlankAndOverridesOnlyWhenAsked(t *testing.T) {
	cases := []struct {
		choice StationChoice
		named  string
		want   string
	}{
		{StationChoice{}, "", ""},
		{StationChoice{}, "PC1", "PC1"},
		{StationChoice{Override: true}, "PC1", "PC1"},
		{StationChoice{Name: "GOTA"}, "", "GOTA"},
		{StationChoice{Name: "GOTA"}, "PC1", "PC1"},
		{StationChoice{Name: "GOTA", Override: true}, "PC1", "GOTA"},
	}
	for _, c := range cases {
		if got := c.choice.For(c.named); got != c.want {
			t.Errorf("%+v For(%q) = %q, want %q", c.choice, c.named, got, c.want)
		}
	}
}

func TestTheServersStationsReachTheStatus(t *testing.T) {
	client := &fakeClient{handshake: func() (api.Handshake, error) {
		handshake := api.Handshake{SupportedAPIVersions: []int{1}, ServerTime: time.Now(), Stations: []api.Station{{Name: "GOTA"}}}
		handshake.Permissions.CreateStations = true
		return handshake, nil
	}}
	h := start(t, client, "26.11.1")

	eventually(t, "the stations", func() bool { return h.sender.Status().StationsKnown })
	if status := h.sender.Status(); len(status.Stations) != 1 || status.Stations[0].Name != "GOTA" || !status.CanCreateStations {
		t.Errorf("status = %+v", status)
	}
}

func TestTheStationIsChosenWhenSendingAndTheQueueKeepsTheLoggersValue(t *testing.T) {
	stations := make(chan string, 1)
	client := &fakeClient{}
	client.setSend(func(contacts []contact.Contact) (map[int]api.Result, error) {
		select {
		case stations <- contacts[0].Station:
		default:
		}
		return map[int]api.Result{0: {Index: 0, Status: "retry", Code: "unknown_station"}}, nil
	})
	h := start(t, client, "26.11.1")
	h.sender.SetStation(StationChoice{Name: "GOTA"})
	h.put(t, "W1AW")

	select {
	case got := <-stations:
		if got != "GOTA" {
			t.Errorf("sent station %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was sent")
	}

	eventually(t, "the queue to keep the logger's value", func() bool {
		attention, err := h.queue.NeedingAttention(context.Background(), 5)
		if err != nil {
			t.Fatal(err)
		}
		return len(attention) == 1 && attention[0].Station == ""
	})
}

func TestAskingAgainShakesHandsWithoutWaiting(t *testing.T) {
	client := &fakeClient{}
	h := startPolling(t, client, "26.11.1", time.Hour)
	shakes := func() int {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.shakes
	}
	eventually(t, "the first handshake", func() bool { return shakes() == 1 })

	h.sender.AskServerAgain()
	eventually(t, "the second handshake", func() bool { return shakes() == 2 })
}
