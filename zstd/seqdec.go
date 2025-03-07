// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"errors"
	"fmt"
	"io"
)

type seq struct {
	litLen   uint32
	matchLen uint32
	offset   uint32

	// Codes are stored here for the encoder
	// so they only have to be looked up once.
	llCode, mlCode, ofCode uint8
}

type seqVals struct {
	ll, ml, mo int
}

func (s seq) String() string {
	if s.offset <= 3 {
		if s.offset == 0 {
			return fmt.Sprint("litLen:", s.litLen, ", matchLen:", s.matchLen+zstdMinMatch, ", offset: INVALID (0)")
		}
		return fmt.Sprint("litLen:", s.litLen, ", matchLen:", s.matchLen+zstdMinMatch, ", offset:", s.offset, " (repeat)")
	}
	return fmt.Sprint("litLen:", s.litLen, ", matchLen:", s.matchLen+zstdMinMatch, ", offset:", s.offset-3, " (new)")
}

type seqCompMode uint8

const (
	compModePredefined seqCompMode = iota
	compModeRLE
	compModeFSE
	compModeRepeat
)

type sequenceDec struct {
	// decoder keeps track of the current state and updates it from the bitstream.
	fse    *fseDecoder
	state  fseState
	repeat bool
}

// init the state of the decoder with input from stream.
func (s *sequenceDec) init(br *bitReader) error {
	if s.fse == nil {
		return errors.New("sequence decoder not defined")
	}
	s.state.init(br, s.fse.actualTableLog, s.fse.dt[:1<<s.fse.actualTableLog])
	return nil
}

// sequenceDecs contains all 3 sequence decoders and their state.
type sequenceDecs struct {
	litLengths   sequenceDec
	offsets      sequenceDec
	matchLengths sequenceDec
	prevOffset   [3]int
	dict         []byte
	literals     []byte
	out          []byte
	nSeqs        int
	br           *bitReader
	seqSize      int
	windowSize   int
	maxBits      uint8
}

// initialize all 3 decoders from the stream input.
func (s *sequenceDecs) initialize(br *bitReader, hist *history, out []byte) error {
	if err := s.litLengths.init(br); err != nil {
		return errors.New("litLengths:" + err.Error())
	}
	if err := s.offsets.init(br); err != nil {
		return errors.New("offsets:" + err.Error())
	}
	if err := s.matchLengths.init(br); err != nil {
		return errors.New("matchLengths:" + err.Error())
	}
	s.br = br
	s.prevOffset = hist.recentOffsets
	s.maxBits = s.litLengths.fse.maxBits + s.offsets.fse.maxBits + s.matchLengths.fse.maxBits
	s.windowSize = hist.windowSize
	s.out = out
	s.dict = nil
	if hist.dict != nil {
		s.dict = hist.dict.content
	}
	return nil
}

// execute will execute the decoded sequence with the provided history.
// The sequence must be evaluated before being sent.
func (s *sequenceDecs) execute(seqs []seqVals, hist []byte) error {
	// Ensure we have enough output size...
	if len(s.out)+s.seqSize > cap(s.out) {
		addBytes := s.seqSize + len(s.out)
		s.out = append(s.out, make([]byte, addBytes)...)
		s.out = s.out[:len(s.out)-addBytes]
	}

	if debugDecoder {
		printf("Execute %d seqs with hist %d, dict %d, literals: %d into %d bytes\n", len(seqs), len(hist), len(s.dict), len(s.literals), s.seqSize)
	}

	var t = len(s.out)
	out := s.out[:t+s.seqSize]

	for _, seq := range seqs {
		// Add literals
		copy(out[t:], s.literals[:seq.ll])
		t += seq.ll
		s.literals = s.literals[seq.ll:]

		// Copy from dictionary...
		if seq.mo > t+len(hist) || seq.mo > s.windowSize {
			if len(s.dict) == 0 {
				return fmt.Errorf("match offset (%d) bigger than current history (%d)", seq.mo, t+len(hist))
			}

			// we may be in dictionary.
			dictO := len(s.dict) - (seq.mo - (t + len(hist)))
			if dictO < 0 || dictO >= len(s.dict) {
				return fmt.Errorf("match offset (%d) bigger than current history+dict (%d)", seq.mo, t+len(hist)+len(s.dict))
			}
			end := dictO + seq.ml
			if end > len(s.dict) {
				n := len(s.dict) - dictO
				copy(out[t:], s.dict[dictO:])
				t += n
				seq.ml -= n
			} else {
				copy(out[t:], s.dict[dictO:end])
				t += end - dictO
				continue
			}
		}

		// Copy from history.
		if v := seq.mo - t; v > 0 {
			// v is the start position in history from end.
			start := len(hist) - v
			if seq.ml > v {
				// Some goes into current block.
				// Copy remainder of history
				copy(out[t:], hist[start:])
				t += v
				seq.ml -= v
			} else {
				copy(out[t:], hist[start:start+seq.ml])
				t += seq.ml
				continue
			}
		}
		// We must be in current buffer now
		if seq.ml > 0 {
			start := t - seq.mo
			if seq.ml <= t-start {
				// No overlap
				copy(out[t:], out[start:start+seq.ml])
				t += seq.ml
				continue
			} else {
				// Overlapping copy
				// Extend destination slice and copy one byte at the time.
				src := out[start : start+seq.ml]
				dst := out[t:]
				dst = dst[:len(src)]
				t += len(src)
				// Destination is the space we just added.
				for i := range src {
					dst[i] = src[i]
				}
			}
		}
	}
	// Add final literals
	copy(out[t:], s.literals)
	if debugDecoder {
		t += len(s.literals)
		if t != len(out) {
			panic(fmt.Errorf("length mismatch, want %d, got %d, ss: %d", len(out), t, s.seqSize))
		}
	}
	s.out = out

	return nil
}

// decode sequences from the stream with the provided history.
func (s *sequenceDecs) decodeSync(hist []byte) error {
	br := s.br
	seqs := s.nSeqs
	startSize := len(s.out)
	// Grab full sizes tables, to avoid bounds checks.
	llTable, mlTable, ofTable := s.litLengths.fse.dt[:maxTablesize], s.matchLengths.fse.dt[:maxTablesize], s.offsets.fse.dt[:maxTablesize]
	llState, mlState, ofState := s.litLengths.state.state, s.matchLengths.state.state, s.offsets.state.state
	out := s.out
	maxBlockSize := maxCompressedBlockSize
	if s.windowSize < maxBlockSize {
		maxBlockSize = s.windowSize
	}

	for i := seqs - 1; i >= 0; i-- {
		if br.overread() {
			printf("reading sequence %d, exceeded available data\n", seqs-i)
			return io.ErrUnexpectedEOF
		}
		var ll, mo, ml int
		if br.off > 4+((maxOffsetBits+16+16)>>3) {
			// inlined function:
			// ll, mo, ml = s.nextFast(br, llState, mlState, ofState)

			// Final will not read from stream.
			var llB, mlB, moB uint8
			ll, llB = llState.final()
			ml, mlB = mlState.final()
			mo, moB = ofState.final()

			// extra bits are stored in reverse order.
			br.fillFast()
			mo += br.getBits(moB)
			if s.maxBits > 32 {
				br.fillFast()
			}
			ml += br.getBits(mlB)
			ll += br.getBits(llB)

			if moB > 1 {
				s.prevOffset[2] = s.prevOffset[1]
				s.prevOffset[1] = s.prevOffset[0]
				s.prevOffset[0] = mo
			} else {
				// mo = s.adjustOffset(mo, ll, moB)
				// Inlined for rather big speedup
				if ll == 0 {
					// There is an exception though, when current sequence's literals_length = 0.
					// In this case, repeated offsets are shifted by one, so an offset_value of 1 means Repeated_Offset2,
					// an offset_value of 2 means Repeated_Offset3, and an offset_value of 3 means Repeated_Offset1 - 1_byte.
					mo++
				}

				if mo == 0 {
					mo = s.prevOffset[0]
				} else {
					var temp int
					if mo == 3 {
						temp = s.prevOffset[0] - 1
					} else {
						temp = s.prevOffset[mo]
					}

					if temp == 0 {
						// 0 is not valid; input is corrupted; force offset to 1
						println("WARNING: temp was 0")
						temp = 1
					}

					if mo != 1 {
						s.prevOffset[2] = s.prevOffset[1]
					}
					s.prevOffset[1] = s.prevOffset[0]
					s.prevOffset[0] = temp
					mo = temp
				}
			}
			br.fillFast()
		} else {
			ll, mo, ml = s.next(br, llState, mlState, ofState)
			br.fill()
		}

		if debugSequences {
			println("Seq", seqs-i-1, "Litlen:", ll, "mo:", mo, "(abs) ml:", ml)
		}

		if ll > len(s.literals) {
			return fmt.Errorf("unexpected literal count, want %d bytes, but only %d is available", ll, len(s.literals))
		}
		size := ll + ml + len(out)
		if size-startSize > maxBlockSize {
			return fmt.Errorf("output (%d) bigger than max block size (%d)", size, maxBlockSize)
		}
		if size > cap(out) {
			// Not enough size, which can happen under high volume block streaming conditions
			// but could be if destination slice is too small for sync operations.
			// over-allocating here can create a large amount of GC pressure so we try to keep
			// it as contained as possible
			used := len(out) - startSize
			addBytes := 256 + ll + ml + used>>2
			// Clamp to max block size.
			if used+addBytes > maxBlockSize {
				addBytes = maxBlockSize - used
			}
			out = append(out, make([]byte, addBytes)...)
			out = out[:len(out)-addBytes]
		}
		if ml > maxMatchLen {
			return fmt.Errorf("match len (%d) bigger than max allowed length", ml)
		}

		// Add literals
		out = append(out, s.literals[:ll]...)
		s.literals = s.literals[ll:]

		if mo == 0 && ml > 0 {
			return fmt.Errorf("zero matchoff and matchlen (%d) > 0", ml)
		}

		if mo > len(out)+len(hist) || mo > s.windowSize {
			if len(s.dict) == 0 {
				return fmt.Errorf("match offset (%d) bigger than current history (%d)", mo, len(out)+len(hist))
			}

			// we may be in dictionary.
			dictO := len(s.dict) - (mo - (len(out) + len(hist)))
			if dictO < 0 || dictO >= len(s.dict) {
				return fmt.Errorf("match offset (%d) bigger than current history (%d)", mo, len(out)+len(hist))
			}
			end := dictO + ml
			if end > len(s.dict) {
				out = append(out, s.dict[dictO:]...)
				ml -= len(s.dict) - dictO
			} else {
				out = append(out, s.dict[dictO:end]...)
				mo = 0
				ml = 0
			}
		}

		// Copy from history.
		// TODO: Blocks without history could be made to ignore this completely.
		if v := mo - len(out); v > 0 {
			// v is the start position in history from end.
			start := len(hist) - v
			if ml > v {
				// Some goes into current block.
				// Copy remainder of history
				out = append(out, hist[start:]...)
				ml -= v
			} else {
				out = append(out, hist[start:start+ml]...)
				ml = 0
			}
		}
		// We must be in current buffer now
		if ml > 0 {
			start := len(out) - mo
			if ml <= len(out)-start {
				// No overlap
				out = append(out, out[start:start+ml]...)
			} else {
				// Overlapping copy
				// Extend destination slice and copy one byte at the time.
				out = out[:len(out)+ml]
				src := out[start : start+ml]
				// Destination is the space we just added.
				dst := out[len(out)-ml:]
				dst = dst[:len(src)]
				for i := range src {
					dst[i] = src[i]
				}
			}
		}
		if i == 0 {
			// This is the last sequence, so we shouldn't update state.
			break
		}

		// Manually inlined, ~ 5-20% faster
		// Update all 3 states at once. Approx 20% faster.
		nBits := llState.nbBits() + mlState.nbBits() + ofState.nbBits()
		if nBits == 0 {
			llState = llTable[llState.newState()&maxTableMask]
			mlState = mlTable[mlState.newState()&maxTableMask]
			ofState = ofTable[ofState.newState()&maxTableMask]
		} else {
			bits := br.get32BitsFast(nBits)
			lowBits := uint16(bits >> ((ofState.nbBits() + mlState.nbBits()) & 31))
			llState = llTable[(llState.newState()+lowBits)&maxTableMask]

			lowBits = uint16(bits >> (ofState.nbBits() & 31))
			lowBits &= bitMask[mlState.nbBits()&15]
			mlState = mlTable[(mlState.newState()+lowBits)&maxTableMask]

			lowBits = uint16(bits) & bitMask[ofState.nbBits()&15]
			ofState = ofTable[(ofState.newState()+lowBits)&maxTableMask]
		}
	}

	// Check if space for literals
	if len(s.literals)+len(s.out)-startSize > maxBlockSize {
		return fmt.Errorf("output (%d) bigger than max block size (%d)", len(s.out), maxBlockSize)
	}

	// Add final literals
	s.out = append(out, s.literals...)
	return br.close()
}

// update states, at least 27 bits must be available.
func (s *sequenceDecs) update(br *bitReader) {
	// Max 8 bits
	s.litLengths.state.next(br)
	// Max 9 bits
	s.matchLengths.state.next(br)
	// Max 8 bits
	s.offsets.state.next(br)
}

var bitMask [16]uint16

func init() {
	for i := range bitMask[:] {
		bitMask[i] = uint16((1 << uint(i)) - 1)
	}
}

// update states, at least 27 bits must be available.
func (s *sequenceDecs) updateAlt(br *bitReader) {
	// Update all 3 states at once. Approx 20% faster.
	a, b, c := s.litLengths.state.state, s.matchLengths.state.state, s.offsets.state.state

	nBits := a.nbBits() + b.nbBits() + c.nbBits()
	if nBits == 0 {
		s.litLengths.state.state = s.litLengths.state.dt[a.newState()]
		s.matchLengths.state.state = s.matchLengths.state.dt[b.newState()]
		s.offsets.state.state = s.offsets.state.dt[c.newState()]
		return
	}
	bits := br.get32BitsFast(nBits)
	lowBits := uint16(bits >> ((c.nbBits() + b.nbBits()) & 31))
	s.litLengths.state.state = s.litLengths.state.dt[a.newState()+lowBits]

	lowBits = uint16(bits >> (c.nbBits() & 31))
	lowBits &= bitMask[b.nbBits()&15]
	s.matchLengths.state.state = s.matchLengths.state.dt[b.newState()+lowBits]

	lowBits = uint16(bits) & bitMask[c.nbBits()&15]
	s.offsets.state.state = s.offsets.state.dt[c.newState()+lowBits]
}

// nextFast will return new states when there are at least 4 unused bytes left on the stream when done.
func (s *sequenceDecs) nextFast(br *bitReader, llState, mlState, ofState decSymbol) (ll, mo, ml int) {
	// Final will not read from stream.
	ll, llB := llState.final()
	ml, mlB := mlState.final()
	mo, moB := ofState.final()

	// extra bits are stored in reverse order.
	br.fillFast()
	mo += br.getBits(moB)
	if s.maxBits > 32 {
		br.fillFast()
	}
	ml += br.getBits(mlB)
	ll += br.getBits(llB)

	if moB > 1 {
		s.prevOffset[2] = s.prevOffset[1]
		s.prevOffset[1] = s.prevOffset[0]
		s.prevOffset[0] = mo
		return
	}
	// mo = s.adjustOffset(mo, ll, moB)
	// Inlined for rather big speedup
	if ll == 0 {
		// There is an exception though, when current sequence's literals_length = 0.
		// In this case, repeated offsets are shifted by one, so an offset_value of 1 means Repeated_Offset2,
		// an offset_value of 2 means Repeated_Offset3, and an offset_value of 3 means Repeated_Offset1 - 1_byte.
		mo++
	}

	if mo == 0 {
		mo = s.prevOffset[0]
		return
	}
	var temp int
	if mo == 3 {
		temp = s.prevOffset[0] - 1
	} else {
		temp = s.prevOffset[mo]
	}

	if temp == 0 {
		// 0 is not valid; input is corrupted; force offset to 1
		println("temp was 0")
		temp = 1
	}

	if mo != 1 {
		s.prevOffset[2] = s.prevOffset[1]
	}
	s.prevOffset[1] = s.prevOffset[0]
	s.prevOffset[0] = temp
	mo = temp
	return
}

func (s *sequenceDecs) next(br *bitReader, llState, mlState, ofState decSymbol) (ll, mo, ml int) {
	// Final will not read from stream.
	ll, llB := llState.final()
	ml, mlB := mlState.final()
	mo, moB := ofState.final()

	// extra bits are stored in reverse order.
	br.fill()
	if s.maxBits <= 32 {
		mo += br.getBits(moB)
		ml += br.getBits(mlB)
		ll += br.getBits(llB)
	} else {
		mo += br.getBits(moB)
		br.fill()
		// matchlength+literal length, max 32 bits
		ml += br.getBits(mlB)
		ll += br.getBits(llB)

	}
	mo = s.adjustOffset(mo, ll, moB)
	return
}

func (s *sequenceDecs) adjustOffset(offset, litLen int, offsetB uint8) int {
	if offsetB > 1 {
		s.prevOffset[2] = s.prevOffset[1]
		s.prevOffset[1] = s.prevOffset[0]
		s.prevOffset[0] = offset
		return offset
	}

	if litLen == 0 {
		// There is an exception though, when current sequence's literals_length = 0.
		// In this case, repeated offsets are shifted by one, so an offset_value of 1 means Repeated_Offset2,
		// an offset_value of 2 means Repeated_Offset3, and an offset_value of 3 means Repeated_Offset1 - 1_byte.
		offset++
	}

	if offset == 0 {
		return s.prevOffset[0]
	}
	var temp int
	if offset == 3 {
		temp = s.prevOffset[0] - 1
	} else {
		temp = s.prevOffset[offset]
	}

	if temp == 0 {
		// 0 is not valid; input is corrupted; force offset to 1
		println("temp was 0")
		temp = 1
	}

	if offset != 1 {
		s.prevOffset[2] = s.prevOffset[1]
	}
	s.prevOffset[1] = s.prevOffset[0]
	s.prevOffset[0] = temp
	return temp
}
