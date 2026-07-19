package scheduler

import (
	"time"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// nowTimestamp — обёртка для удобства.
func nowTimestamp() *timestamppb.Timestamp {
	return timestamppb.New(time.Now())
}

// cloneProcessInfo — deep clone для безопасного отдавания наружу.
func cloneProcessInfo(src *etroniumv1.ProcessInfo) *etroniumv1.ProcessInfo {
	if src == nil {
		return nil
	}
	return proto.Clone(src).(*etroniumv1.ProcessInfo)
}
