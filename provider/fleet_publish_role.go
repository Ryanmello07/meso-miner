// A fleet commitment written from a validator (source) hotkey is a permanent
// SN25 steering halt, so publication refuses those hotkeys before signing.
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"

	snchain "github.com/urfoundation/sn/chain"
	"github.com/urfoundation/sn/crv4"
)

// ErrFleetPublishValidatorHotkey is returned when the manifest hotkey holds a
// validator role on the manifest subnet.
var ErrFleetPublishValidatorHotkey = errors.New("fleet publish refused: the hotkey is a validator source hotkey on this subnet")

// refuseFleetPublishFromValidatorRole reads the hotkey's role at one
// finalized block through a view bound to metadata authenticated for that
// block. It refuses when the hotkey is the subnet owner hotkey or its UID
// holds a validator permit: the two ways the runtime lets a hotkey set
// weights (see crv4.ValidatorStakeObservation.MeetsNonSelfStakeAndPermit).
// The stake threshold is not consulted, so a permit holder is refused even
// below it.
//
// Why: the commitment oracle mirrors tagged fleet commitments into
// STCoordinator's mirroredCommitments, where the contract cannot clear a
// mirror. A validator accepts a mirror of its source hotkey only when it
// copies its own source write (checkSourceRoleMirrorV2), and its native slot
// must hold one of its own source intents, or checkSourceRoleV2 returns
// "validator source native slot belongs to another role or unretained write".
// A fleet write from that hotkey therefore halts steering for good. This
// writer does not emit the tagged format yet; refusing the same roles here
// keeps every signing path fail closed ahead of that port. No validator has
// a reason to publish a fleet, so there is no override.
//
// Any read error refuses the publish (fail closed). The check is at a
// finalized block, so a validator whose permit has lapsed and that is not
// the owner hotkey is not detected; it cannot set weights while that lasts.
func refuseFleetPublishFromValidatorRole(ctx context.Context, view *crv4.Chain, netuid uint16, hotkey [32]byte, block types.Hash) error {
	if ctx == nil || view == nil || block == (types.Hash{}) {
		return fmt.Errorf("%w: role check dependencies are unavailable", ErrFleetPublishValidatorHotkey)
	}
	owner, ownerPresent, err := subnetOwnerHotkeyAtContext(ctx, view, netuid, block)
	if err != nil {
		return fmt.Errorf("fleet publish refused: read the netuid %d owner hotkey at %s: %w", netuid, block.Hex(), err)
	}
	if ownerPresent && owner == hotkey {
		return fmt.Errorf("%w: 0x%x is the netuid %d subnet owner hotkey (a fleet commitment from it would be mirrored and halt the validator)", ErrFleetPublishValidatorHotkey, hotkey, netuid)
	}
	uid, registered, err := snchain.UIDAtContext(ctx, view, netuid, hotkey, block)
	if err != nil {
		return fmt.Errorf("fleet publish refused: read the netuid %d UID of 0x%x at %s: %w", netuid, hotkey, block.Hex(), err)
	}
	if !registered {
		return ctx.Err()
	}
	permit, err := snchain.ValidatorPermitAtContext(ctx, view, netuid, uid, block)
	if err != nil {
		return fmt.Errorf("fleet publish refused: read the netuid %d validator permit of UID %d at %s: %w", netuid, uid, block.Hex(), err)
	}
	if permit {
		return fmt.Errorf("%w: 0x%x (netuid %d UID %d) holds a validator permit (a fleet commitment from it would be mirrored and halt the validator)", ErrFleetPublishValidatorHotkey, hotkey, netuid, uid)
	}
	return ctx.Err()
}

// subnetOwnerHotkeyAtContext reads SubtensorModule.SubnetOwnerHotkey at one
// block. The pinned sn predates snchain.SubnetOwnerHotkeyAtContext, so the
// read is local until a pin bump replaces it.
func subnetOwnerHotkeyAtContext(ctx context.Context, chain *crv4.Chain, netuid uint16, blockHash types.Hash) ([32]byte, bool, error) {
	if chain == nil || chain.Meta == nil {
		return [32]byte{}, false, errors.New("storage metadata is unavailable")
	}
	key, err := types.CreateStorageKey(chain.Meta, crv4.PalletName, "SubnetOwnerHotkey", snchain.NetuidArg(netuid))
	if err != nil {
		return [32]byte{}, false, fmt.Errorf("storage key %s.SubnetOwnerHotkey: %w", crv4.PalletName, err)
	}
	var owner types.AccountID
	present, err := snchain.ReadStorageAtContext(ctx, chain, key, crv4.PalletName, "SubnetOwnerHotkey", &owner, blockHash)
	if err != nil || !present {
		return [32]byte{}, false, err
	}
	return [32]byte(owner), true, nil
}
