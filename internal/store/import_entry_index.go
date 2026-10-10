package store

import "math"

// importEntryIndex retains generated entry IDs as their 80-bit payload and
// interns transcript IDs. Legacy IDs retain ordinary map set/get semantics.
// Its zero value is ready to use.
type importEntryIndex struct {
	compact     map[[10]byte]uint32
	fallback    map[string]string
	ordinals    map[string]uint32
	transcripts []string
	maxOrdinals uint32
}

var importEntryAlphabet = [32]byte{
	'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h',
	'i', 'j', 'k', 'l', 'm', 'n', 'o', 'p',
	'q', 'r', 's', 't', 'u', 'v', 'w', 'x',
	'y', 'z', '2', '3', '4', '5', '6', '7',
}

func importEntryKey(entryID string) (key [10]byte, ok bool) {
	if len(entryID) != 20 || entryID[:4] != "ent_" {
		return key, false
	}
	var bits uint32
	var count uint
	position := 0
	for i := 4; i < len(entryID); i++ {
		c := entryID[i]
		value := c - 'a'
		if c >= '2' && c <= '7' {
			value = c - '2' + 26
		}
		if value >= byte(len(importEntryAlphabet)) || importEntryAlphabet[value] != c {
			return key, false
		}
		bits = bits<<5 | uint32(value)
		count += 5
		if count >= 8 {
			count -= 8
			key[position] = byte(bits >> count)
			position++
		}
	}
	return key, true
}

func (index *importEntryIndex) put(entryID, transcriptID string) {
	if index.maxOrdinals == 0 {
		index.maxOrdinals = math.MaxUint32
	}
	key, compact := importEntryKey(entryID)
	if compact && uint64(len(index.transcripts)) < uint64(index.maxOrdinals) {
		if index.compact == nil {
			index.compact = make(map[[10]byte]uint32)
			index.ordinals = make(map[string]uint32)
		}
		ordinal, found := index.ordinals[transcriptID]
		if !found {
			ordinal = uint32(len(index.transcripts))
			index.ordinals[transcriptID] = ordinal
			index.transcripts = append(index.transcripts, transcriptID)
		}
		index.compact[key] = ordinal
		delete(index.fallback, entryID)
		return
	}
	if index.fallback == nil {
		index.fallback = make(map[string]string)
	}
	index.fallback[entryID] = transcriptID
	if compact {
		delete(index.compact, key)
	}
}

func (index *importEntryIndex) transcriptOf(entryID string) (string, bool) {
	if key, compact := importEntryKey(entryID); compact {
		if ordinal, found := index.compact[key]; found {
			return index.transcripts[ordinal], true
		}
	}
	transcriptID, found := index.fallback[entryID]
	return transcriptID, found
}
