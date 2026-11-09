package main

import (
	"testing"
	"time"
)

// resetListingCache clears the account listing cache so a test reads the
// catalogue its own sender answers with.
func resetListingCache() {
	listingCache.Lock()
	listingCache.entries = nil
	listingCache.Unlock()
}

// A job token past its recorded expiry must not shadow a device token that still
// works: the account keeps one live credential, and signing with the dead one
// makes the catalogue and every answer look like a lapsed sign-in.
func TestChatTokenGivesWayToDeviceTokenWhenJobTokenExpired(t *testing.T) {
	live := credential{Access: "jt-live", DeviceToken: "dt-device", Expires: time.Now().Add(time.Hour).UnixMilli()}
	if got := live.chatToken(); got != "jt-live" {
		t.Fatalf("live job token: got %q", got)
	}
	expired := live
	expired.Expires = time.Now().Add(-time.Minute).UnixMilli()
	if got := expired.chatToken(); got != "dt-device" {
		t.Fatalf("expired job token: got %q", got)
	}
	expired.DeviceToken = ""
	if got := expired.chatToken(); got != "jt-live" {
		t.Fatalf("expired job token with no device token: got %q", got)
	}
	deviceChat := live
	deviceChat.DeviceChat = true
	if got := deviceChat.chatToken(); got != "dt-device" {
		t.Fatalf("device-chat account: got %q", got)
	}
	unknownExpiry := credential{Access: "jt-live", DeviceToken: "dt-device"}
	if got := unknownExpiry.chatToken(); got != "jt-live" {
		t.Fatalf("job token of unknown expiry: got %q", got)
	}
}

// The retry only exists when it changes which token signs the call.
func TestWithDeviceTokenOnlyWhenItChangesTheSignature(t *testing.T) {
	live := credential{Access: "jt-live", DeviceToken: "dt-device", Refresh: "rt", DeviceRefresh: "drt", Expires: time.Now().Add(time.Hour).UnixMilli()}
	retry, ok := live.withDeviceToken()
	if !ok || !retry.DeviceChat || retry.chatToken() != "dt-device" {
		t.Fatalf("retry credential = %+v, ok = %v", retry, ok)
	}
	if retry.Access != live.Access || retry.DeviceToken != live.DeviceToken {
		t.Fatalf("retry rewrote the credential: %+v", retry)
	}
	if _, ok := (credential{Access: "jt-live", Expires: live.Expires}).withDeviceToken(); ok {
		t.Fatal("a credential with no device token offered a retry")
	}
	expired := live
	expired.Expires = time.Now().Add(-time.Minute).UnixMilli()
	if _, ok := expired.withDeviceToken(); ok {
		t.Fatal("a credential already signing with its device token offered a retry")
	}
	if _, ok := (credential{Access: "jt-live", DeviceToken: "dt-device", DeviceChat: true}).withDeviceToken(); ok {
		t.Fatal("a device-chat credential offered a retry")
	}
}
