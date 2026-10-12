package connect

import "testing"

func TestTransferSendStats_SplitsFirstSendsFromResends(t *testing.T) {
	before := TransferSendStatsNow()

	recordTransferFirstSend(1000)
	recordTransferFirstSend(500)
	recordTransferResend(1000, 2)
	recordTransferResend(1000, 5)
	recordTransferResend(1000, 16)

	got := TransferSendStatsNow()
	if d := got.FirstFrames - before.FirstFrames; d != 2 {
		t.Fatalf("first frames +%d, want 2", d)
	}
	if d := got.FirstBytes - before.FirstBytes; d != 1500 {
		t.Fatalf("first bytes +%d, want 1500", d)
	}
	if d := got.ResendFrames - before.ResendFrames; d != 3 {
		t.Fatalf("resend frames +%d, want 3", d)
	}
	if d := got.ResendBytes - before.ResendBytes; d != 3000 {
		t.Fatalf("resend bytes +%d, want 3000", d)
	}
	// the depth buckets say how many times an item had already been sent
	if got.ResendDepth[0]-before.ResendDepth[0] != 1 { // 2nd send
		t.Fatalf("depth bucket 2 = %+v", got.ResendDepth)
	}
	if got.ResendDepth[2]-before.ResendDepth[2] != 1 { // 5th send (5-8)
		t.Fatalf("depth bucket 5-8 = %+v", got.ResendDepth)
	}
	if got.ResendDepth[3]-before.ResendDepth[3] != 1 { // 16th send (9+)
		t.Fatalf("depth bucket 9+ = %+v", got.ResendDepth)
	}
}
