// Copyright (C) 2022 Sneller, Inc.
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package vm

import _ "unsafe"

// The Unsafe variants assume all the parameters are valid. If pre-validation is required, it should be provided by the respective wrappers.

// Takes a single uint16 parameter denoting opcode ID and returns the address of the associated handler.
//
//go:noescape
//go:norace
//go:nosplit
func getOpcodeAddressUnsafe(op uint16) uintptr

//go:linkname getOpcodeAddressUnsafe x64asm_getOpcodeAddressUnsafe

func getOpcodeAddress(op bcop) uintptr {

	return getOpcodeAddressUnsafe(uint16(op))
}

func getOpcodeAddressAsArray(op bcop) []byte {

	addr := getOpcodeAddress(op)
	return []byte{byte(addr >> 0), byte(addr >> 8), byte(addr >> 16), byte(addr >> 24), byte(addr >> 32), byte(addr >> 40), byte(addr >> 48), byte(addr >> 56)}
}

// Reverse mapping from opcode adresses to opcode IDs. For pretty printers, serializers etc.

var opcodeToIdMapSingleton map[uintptr]bcop

func init() {

	opcodeToIdMapSingleton = make(map[uintptr]bcop)

	for i := 0; i != _maxbcop; i++ {

		id := bcop(i)
		addr := getOpcodeAddress(id)
		opcodeToIdMapSingleton[addr] = id
	}
}

func getOpcodeID(addr uintptr) (bcop, bool) {

	val, present := opcodeToIdMapSingleton[addr]
	return val, present
}
