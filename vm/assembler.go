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

import (

    "fmt"
)

type assembler struct {

    content []byte
}

func (a* assembler) getContent() []byte {

    return a.content
}

func (a* assembler) grabContent() []byte {

    r := a.content
    return r
}

func (a* assembler) emitImmU8(imm uint8) {

    a.content = append(a.content, byte(imm))
}

func (a* assembler) emitImmI8(imm int8) {

    a.emitImmU8(uint8(imm))
}

func (a* assembler) emitImmU16(imm uint16) {

	a.content = append(a.content, byte(imm), byte(imm>>8))
}

func (a* assembler) emitImmI16(imm int16) {

    a.emitImmU16(uint16(imm))
}

func (a* assembler) emitImmU32(imm uint32) {

	a.content = append(a.content, byte(imm), byte(imm>>8), byte(imm>>16), byte(imm>>24))
}

func (a* assembler) emitImmI32(imm int32) {

    a.emitImmU32(uint32(imm))
}

func (a* assembler) emitImmU64(imm uint64) {

	a.content = append(a.content, byte(imm), byte(imm>>8), byte(imm>>16), byte(imm>>24), byte(imm>>32), byte(imm>>40), byte(imm>>48), byte(imm>>56))
}

func (a* assembler) emitImmI64(imm int64) {

    a.emitImmU64(uint64(imm))
}

func (a* assembler) emitImmUPtr(imm uintptr) {

	a.emitImmU64(uint64(imm))
}

func (a* assembler) emitOpcode(op bcop) {

	addr := getOpcodeAddress(op)
	fmt.Printf("emitOpcode %d, address=%x\n", op, addr)

	//c.instrs = append(c.instrs, byte(op), byte(op>>8))
	//c.emitImmU16(uint16(op))
	a.emitImmUPtr(addr)
}

func opcodeToBytes(op bcop) []byte {

    asm := new(assembler)
    asm.emitOpcode(op)
    return asm.grabContent()
}
