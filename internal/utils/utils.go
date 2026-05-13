//  Copyright 2026 Google LLC
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package utils

import (
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
)

// ProtoToTextString converts a proto message to a textproto string.
func ProtoToTextString(msg proto.Message) (string, error) {
	marshaler := prototext.MarshalOptions{
		Indent: "  ", // Optional: for pretty printing
	}
	b, err := marshaler.Marshal(msg)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// TextStringToProto converts a textproto string to a proto message.
func TextStringToProto(textString string, msg proto.Message) error {
	unmarshaler := prototext.UnmarshalOptions{}
	err := unmarshaler.Unmarshal([]byte(textString), msg)
	if err != nil {
		return err
	}
	return nil
}
