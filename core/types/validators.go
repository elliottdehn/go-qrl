// Copyright 2026 The QRL Authors
// This file is part of go-qrl.
//
// SPDX-License-Identifier: LGPL-3.0-or-later

package types

import (
	"bytes"

	"github.com/theQRL/go-qrl/common"
	"github.com/theQRL/go-qrl/rlp"
)

// Validators implements DerivableList for the active PoS validator
// set carried in a block body. Used to compute Header.ValidatorsHash.
type Validators []common.Address

// Len returns the length of s.
func (s Validators) Len() int { return len(s) }

// EncodeIndex encodes the i'th validator address to w. Each address
// is RLP-encoded as a 20-byte string.
func (s Validators) EncodeIndex(i int, w *bytes.Buffer) {
	rlp.Encode(w, s[i])
}
