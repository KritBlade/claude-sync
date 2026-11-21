package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Antigravity stores conversation steps as protobuf without a published schema. Paths sit in string fields,
// and every string carries its length in front of it, so a path cannot be replaced in place. This file walks
// the wire format generically and re-encodes only the fields whose text it changes.

type protobufField struct {
	start, tagEnd, payloadStart, end int
	fieldNumber                      uint64
	wireType                         uint64
}

var errNotProtobuf = errors.New("not a protobuf message")

// parseProtobufFields splits data into fields, or returns errNotProtobuf when data is not a well-formed
// message (deprecated group wire types are treated as not well-formed).
func parseProtobufFields(data []byte) ([]protobufField, error) {
	var fields []protobufField
	position := 0
	for position < len(data) {
		start := position
		key, size := binary.Uvarint(data[position:])
		if size <= 0 {
			return nil, errNotProtobuf
		}
		position += size
		field := protobufField{start: start, tagEnd: position, fieldNumber: key >> 3, wireType: key & 7}
		if field.fieldNumber == 0 || field.fieldNumber > 1<<29-1 {
			return nil, errNotProtobuf
		}
		switch field.wireType {
		case 0:
			_, valueSize := binary.Uvarint(data[position:])
			if valueSize <= 0 {
				return nil, errNotProtobuf
			}
			field.payloadStart = position
			position += valueSize
		case 1:
			field.payloadStart = position
			position += 8
		case 5:
			field.payloadStart = position
			position += 4
		case 2:
			length, lengthSize := binary.Uvarint(data[position:])
			if lengthSize <= 0 || length > uint64(len(data)) {
				return nil, errNotProtobuf
			}
			field.payloadStart = position + lengthSize
			position = field.payloadStart + int(length)
		default:
			return nil, errNotProtobuf
		}
		if position > len(data) {
			return nil, errNotProtobuf
		}
		field.end = position
		fields = append(fields, field)
	}
	return fields, nil
}

// isCleanText reports whether data is UTF-8 text with no control characters other than tab, CR and LF.
func isCleanText(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for _, b := range data {
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' {
			return false
		}
	}
	return true
}

// protobufTextConverter applies a text conversion to every string field that needs it, at any depth.
// A length-delimited field is walked as a nested message when it parses as one, converted as text when it
// is clean text, and otherwise left as it is and counted in unhandledFields.
type protobufTextConverter struct {
	convertText     func([]byte) []byte
	hasCandidate    func([]byte) bool
	unhandledFields int
}

// convertBlob converts one stored value: a protobuf message, or plain text.
func (converter *protobufTextConverter) convertBlob(data []byte) ([]byte, error) {
	if !converter.hasCandidate(data) {
		return data, nil
	}
	fields, err := parseProtobufFields(data)
	if err == nil && len(fields) > 0 {
		converted := converter.convertMessage(data, fields)
		convertedFields, err := parseProtobufFields(converted)
		if err != nil || !haveSameLayout(fields, convertedFields) {
			return nil, fmt.Errorf("message layout changed during path conversion")
		}
		return converted, nil
	}
	if isCleanText(data) {
		return converter.convertText(data), nil
	}
	converter.unhandledFields++
	return data, nil
}

func (converter *protobufTextConverter) convertNested(payload []byte) []byte {
	fields, err := parseProtobufFields(payload)
	if err == nil && len(fields) > 0 {
		return converter.convertMessage(payload, fields)
	}
	if isCleanText(payload) {
		return converter.convertText(payload)
	}
	converter.unhandledFields++
	return payload
}

// convertMessage copies untouched fields byte for byte and re-encodes the length of each changed one.
func (converter *protobufTextConverter) convertMessage(data []byte, fields []protobufField) []byte {
	out := make([]byte, 0, len(data))
	for _, field := range fields {
		if field.wireType == 2 {
			payload := data[field.payloadStart:field.end]
			if converter.hasCandidate(payload) {
				converted := converter.convertNested(payload)
				if !bytes.Equal(converted, payload) {
					out = append(out, data[field.start:field.tagEnd]...)
					out = binary.AppendUvarint(out, uint64(len(converted)))
					out = append(out, converted...)
					continue
				}
			}
		}
		out = append(out, data[field.start:field.end]...)
	}
	return out
}

func haveSameLayout(before, after []protobufField) bool {
	if len(before) != len(after) {
		return false
	}
	for i := range before {
		if before[i].fieldNumber != after[i].fieldNumber || before[i].wireType != after[i].wireType {
			return false
		}
	}
	return true
}
