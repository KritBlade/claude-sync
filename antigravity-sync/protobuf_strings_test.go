package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func encodeField(fieldNumber uint64, wireType uint64, payload []byte) []byte {
	out := binary.AppendUvarint(nil, fieldNumber<<3|wireType)
	if wireType == 2 {
		out = binary.AppendUvarint(out, uint64(len(payload)))
	}
	return append(out, payload...)
}

func encodeVarintField(fieldNumber, value uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, fieldNumber<<3), value)
}

func fieldPayload(t *testing.T, message []byte, fieldNumber uint64) []byte {
	t.Helper()
	fields, err := parseProtobufFields(message)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, field := range fields {
		if field.fieldNumber == fieldNumber {
			return message[field.payloadStart:field.end]
		}
	}
	t.Fatalf("field %d not found", fieldNumber)
	return nil
}

func TestNestedPathStringsAreConvertedWithNewLengths(t *testing.T) {
	converter := windowsConverter(t)
	inner := append(encodeField(1, 2, []byte(`h:\Github\Dev\VectFox`)), encodeVarintField(2, 5)...)
	binaryBlob := []byte{0xff, 0x00, 0x10, 'H', ':', '\\', 'G'}
	outer := append(encodeField(1, 2, inner), encodeField(3, 2, []byte("no path here"))...)
	outer = append(outer, encodeField(4, 2, binaryBlob)...)

	textConverter := &protobufTextConverter{
		convertText:  func(data []byte) []byte { converted, _ := converter.toPortable(data); return converted },
		hasCandidate: converter.containsLocalRoot,
	}
	converted, err := textConverter.convertBlob(outer)
	if err != nil {
		t.Fatal(err)
	}
	if got := fieldPayload(t, fieldPayload(t, converted, 1), 1); string(got) != `${PORTABLE_PATH:WORK}/VectFox` {
		t.Errorf("nested string = %s", got)
	}
	if got := fieldPayload(t, converted, 3); string(got) != "no path here" {
		t.Errorf("untouched string = %s", got)
	}
	if got := fieldPayload(t, converted, 4); !bytes.Equal(got, binaryBlob) {
		t.Errorf("binary field changed: %v", got)
	}
}

func TestNonProtobufTextIsConvertedAsText(t *testing.T) {
	converter := windowsConverter(t)
	textConverter := &protobufTextConverter{
		convertText:  func(data []byte) []byte { converted, _ := converter.toPortable(data); return converted },
		hasCandidate: converter.containsLocalRoot,
	}
	converted, err := textConverter.convertBlob([]byte(`{"Cwd":"H:\\Github\\Dev\\VectFox"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(converted) != `{"Cwd":"${PORTABLE_PATH:WORK}/VectFox"}` {
		t.Errorf("got %s", converted)
	}
}

func TestMalformedMessagesAreRejected(t *testing.T) {
	for _, data := range [][]byte{
		{0x0a, 0x05, 'a'},        // length past the end
		{0x0b},                   // wire type 3 (group)
		{0x00, 0x01},             // field number 0
		{0x08, 0xff, 0xff, 0xff}, // unterminated varint
	} {
		if _, err := parseProtobufFields(data); err == nil {
			t.Errorf("parseProtobufFields(%v) accepted malformed data", data)
		}
	}
}
