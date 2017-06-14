package spvfallback

import (
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/chain/rpc"
	"github.com/btcsuite/btcwallet/chain/spv"
	"github.com/btcsuite/btcwallet/wtxmgr"
)

// This test is designed to prove convincingly that the fallback type will
//   (a) Transmit block notifications from either the rpc struct or the
//       spv struct, whichever comes first.
//   (b) will activate spv notifications in the event that rpc
//       notifications fall behind.
//   (c)

// The strategy for this test is to start a fallback type which does
// not hold a real rpc.RPCClient or spv.SPV, but which we can still
// use to test

func makeTestContext() *fallback {
	rpc := (*rpc.RPCClient)(nil)
	spv := (*spv.SPV)(nil)
	return &fallback{
		rpcChain: rpc,
		spvChain: spv,
		primary:  rpc,
		quit:     make(chan struct{}),
	}
}

// A crude way of testing two notifications for equality.
func isEqual(a, b chain.Notification) bool {
	switch m := a.(type) {
	case chain.RelevantTx:
		var p, q chain.RelevantTx
		p = m

		switch n := b.(type) {
		case chain.RelevantTx:
			q = n
		default:
			return false
		}

		return p.TxRecord == q.TxRecord
	case chain.BlockConnected:
		var p, q chain.BlockConnected
		p = m

		switch n := b.(type) {
		case chain.BlockConnected:
			q = n
		default:
			return false
		}

		return p.Block == q.Block
	case chain.BlockDisconnected:
		var p, q chain.BlockDisconnected
		p = m

		switch n := b.(type) {
		case chain.BlockDisconnected:
			q = n
		default:
			return false
		}

		return p.Block == q.Block
	default:
		return false
	}
}

func testStartFallback(f *fallback) (rpcEnqueue, spvEnqueue, dequeue chan chain.Notification) {
	f.wg.Add(2)
	rpcEnqueue = make(chan chain.Notification)
	spvEnqueue = make(chan chain.Notification)
	dequeue = make(chan chain.Notification, 1)
	queue := make(chan chain.Notification)
	go f.inHandler(2, rpcEnqueue, spvEnqueue, queue)
	go f.outHandler(queue, dequeue, f.quit)
	return
}

type FallbackTester struct {
	rpcEnqueue, spvEnqueue chan chain.Notification
	dequeue                chan chain.Notification
	rpcClosed, spvClosed   bool
	testNo                 int
}

func (ft *FallbackTester) SendRPC(n chain.Notification) {
	if ft.rpcClosed {
		panic("Illegal rpc send after close")
	}

	ft.rpcEnqueue <- n
}

func (ft *FallbackTester) SendSPV(n chain.Notification) {
	if ft.spvClosed {
		panic("Illegal spv send after close")
	}

	ft.spvEnqueue <- n
}

func (ft *FallbackTester) Read(n chain.Notification, t *testing.T, err string) {
	var x chain.Notification
	var ok bool
	select {
	case <-time.After(time.Second):
		t.Errorf("Test %d; No notification read: %v", ft.testNo, err)
		return
	case x, ok = <-ft.dequeue:
	}
	if !ok {
		panic("Test error: dequeue should not be closed as part of a test.")
	}
	if x == nil {
		t.Errorf("Test %d; Nil notification received.", ft.testNo)
	}
	if !isEqual(n, x) {
		t.Errorf("Test %d fail; %v", ft.testNo, err)
	}
}

func (ft *FallbackTester) CloseRPC() {
	if ft.rpcClosed {
		return
	}

	ft.rpcClosed = true
	close(ft.rpcEnqueue)
}

func (ft *FallbackTester) CloseSPV() {
	if ft.spvClosed {
		return
	}

	ft.spvClosed = true
	close(ft.spvEnqueue)
}

func NewFallbackTester(f *fallback, i int) *FallbackTester {
	rpcEnqueue, spvEnqueue, dequeue := testStartFallback(f)
	return &FallbackTester{
		rpcEnqueue: rpcEnqueue,
		spvEnqueue: spvEnqueue,
		dequeue:    dequeue,
		testNo:     i,
	}
}

func MakeTestNotification() chain.Notification {
	return chain.RelevantTx{TxRecord: &wtxmgr.TxRecord{}}
}

func MakeBlockNotification(height uint32) chain.BlockConnected {
	n := chain.BlockConnected(wtxmgr.BlockMeta{
		Block: wtxmgr.Block{
			Height: int32(height),
		},
		Time: time.Now(),
	})

	r := make([]byte, 32)
	n.Hash.SetBytes(r)

	return n
}

type FallbackTest func(t *testing.T, ft *FallbackTester)

var FallbackTests = []FallbackTest{
	// Test that a message from the primary channel is transmitted through.
	func(t *testing.T, ft *FallbackTester) {
		nr := MakeTestNotification()
		ns := MakeTestNotification()
		ft.SendRPC(nr)
		ft.SendSPV(ns)
		ft.Read(nr, t, "Test 1: could not read notification")
	},
	// Close the secondary channel and ensure the primary channel continues to run.
	func(t *testing.T, ft *FallbackTester) {
		ft.CloseSPV()
		n := MakeTestNotification()
		ft.SendRPC(n)
		ft.Read(n, t, "Test 2: could not read notification")
	},
	// Close the primary channel and insure that we switch to the secondary channel.
	func(t *testing.T, ft *FallbackTester) {
		n := MakeTestNotification()
		ft.CloseRPC()
		ft.SendSPV(n)
		ft.Read(n, t, "Test 3: could not read notification")
	},
	// Same thing but a different order, just in case!
	func(t *testing.T, ft *FallbackTester) {
		n := MakeTestNotification()
		ft.SendSPV(n)
		ft.CloseRPC()
		ft.Read(n, t, "Test 4: could not read notification")
	},
	// Test that a message from a secondary channel is transmitted after
	// fallback mode is initiated.
	func(t *testing.T, ft *FallbackTester) {
		n1 := MakeTestNotification()
		b3 := MakeBlockNotification(3)
		b4 := MakeBlockNotification(4)
		b5 := MakeBlockNotification(5)

		// Send a block notifications by both channels so that they are caught up.
		ft.SendRPC(b3)
		ft.SendSPV(b3)

		// Send a notification that will be received after fallback mode is
		// initiated.
		ft.SendSPV(n1)

		// Initiate fallback mode.
		ft.SendSPV(b4)
		ft.SendSPV(b5)

		// First we should get a notification for block three.
		ft.Read(b3, t, "Failed to read first block notification")

		// Then we should read everything we sent through spv.
		ft.Read(n1, t, "Failed to read tx notification")
		ft.Read(b4, t, "Failed to read second block notification")
		ft.Read(b5, t, "Failed to read third block notification")

		// We should now read two block disconnected messages.
		ft.Read((chain.BlockDisconnected)(b5), t, "Failed to read first block disconnection")
		ft.Read((chain.BlockDisconnected)(b4), t, "Failed to read second block disconnection")
	},
	// Same thing with a different order.
	func(t *testing.T, ft *FallbackTester) {
		n1 := MakeTestNotification()
		b3 := MakeBlockNotification(3)
		b4 := MakeBlockNotification(4)
		b5 := MakeBlockNotification(5)

		// Send a block notifications by both channels so that they are caught up.
		ft.SendSPV(b3)
		ft.SendRPC(b3)

		// Send a notification that will be received after fallback mode is
		// initiated.
		ft.SendSPV(n1)

		// Initiate fallback mode.
		ft.SendSPV(b4)
		ft.SendSPV(b5)

		// First we should get a notification for block three.
		ft.Read(b3, t, "Failed to read first block notification")

		// Then we should read everything we sent through spv.
		ft.Read(n1, t, "Failed to read tx notification")
		ft.Read(b4, t, "Failed to read second block notification")
		ft.Read(b5, t, "Failed to read third block notification")

		// We should now read two block disconnected messages.
		ft.Read((chain.BlockDisconnected)(b5), t, "Failed to read first block disconnection")
		ft.Read((chain.BlockDisconnected)(b4), t, "Failed to read second block disconnection")
	},
	// Test that a message from a secondary channel is transmitted after
	// fallback mode is initiated, when no block notification has been received
	// by the primary channel at all.
	func(t *testing.T, ft *FallbackTester) {
		n1 := MakeTestNotification()
		b4 := MakeBlockNotification(4)
		b5 := MakeBlockNotification(5)

		// Send a notification that will be received after fallback mode is
		// initiated.
		ft.SendSPV(n1)

		// Initiate fallback mode.
		ft.SendSPV(b4)
		ft.SendSPV(b5)

		// We should read the three notifications now.
		ft.Read(n1, t, "ablA")
		ft.Read(b4, t, "ablB")
		ft.Read(b5, t, "ablC")

		// We should now read two block disconnected messages.
		ft.Read((chain.BlockDisconnected)(b5), t, "Failed to read first block disconnection")
		ft.Read((chain.BlockDisconnected)(b4), t, "Failed to read second block disconnection")
	},
	// Initiate fallback mode, and then allow the primary channel to
	// catch up again and check that the primary channel is reinstated.
	func(t *testing.T, ft *FallbackTester) {
		n2 := MakeTestNotification()
		b3 := MakeBlockNotification(3)
		b4 := MakeBlockNotification(4)
		b5 := MakeBlockNotification(5)

		// Send a block notifications by both channels so that they are caught up.
		ft.SendSPV(b3)
		ft.SendRPC(b3)

		// Initiate fallback mode.
		ft.SendSPV(b4)
		ft.SendSPV(b5)

		// We should read the three notifications now.
		ft.Read(b3, t, "Failed to read the first block message.")
		ft.Read(b4, t, "Failed to read the first block message.")
		ft.Read(b5, t, "Failed to read the second block message. ")

		// We should now read two block disconnected messages.
		ft.Read((chain.BlockDisconnected)(b5), t, "Failed to read first block disconnection")
		ft.Read((chain.BlockDisconnected)(b4), t, "Failed to read second block disconnection")

		// Send a notification that will be received after fallback mode is
		// initiated.
		ft.SendRPC(n2)

		// Resend the two blocks over rpc so now rpc is ahead.
		ft.SendRPC(b4)
		ft.SendRPC(b5)

		ft.Read(n2, t, "blA")
		ft.Read(b4, t, "Failed to read third block message.")
		ft.Read(b5, t, "Failed to read fourth block message. ")
	},
	// In the event of an orphan block, the fallback manager should reset
	// and act as if it has never seen any block notifications.
	func(t *testing.T, ft *FallbackTester) {
		n1 := MakeTestNotification()
		b3 := MakeBlockNotification(3)
		b4 := MakeBlockNotification(4)
		b5 := MakeBlockNotification(5)
		b6 := MakeBlockNotification(6)

		ft.SendRPC(b3)
		ft.SendSPV(b3)

		ft.SendRPC(b4)
		ft.SendSPV(b4)

		ft.SendSPV(b5)
		ft.SendRPC((chain.BlockDisconnected)(b4))

		// This would normally have initiated fallback mode because
		// it's two blocks ahead, but because of the block disconnection
		// we reset.
		ft.SendSPV(n1)
		ft.SendSPV(b6)

		ft.Read(b3, t, "Failed to read first block connected notifiation")
		ft.Read(b4, t, "Failed to read second block connected notification")
		ft.Read((chain.BlockDisconnected)(b4), t, "Failed to read block disconnected notification.")
	},
}

func TestFallback(t *testing.T) {
	for i, test := range FallbackTests {
		fallback := makeTestContext()

		test(t, NewFallbackTester(fallback, i))

		// The test is done, so stop the fallback object.
		fallback.Stop()
	}
}
