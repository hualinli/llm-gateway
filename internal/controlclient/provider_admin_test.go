package controlclient

import (
	"errors"
	"testing"

	pb "llm-gateway/proto/control"
)

func TestProviderRouteConversionsPreserveDisabledModelAsEmptyArray(t *testing.T) {
	routes := ProviderRouteMap{"disabled-model": {}}
	protoRoutes := providerRoutesToProto(routes)
	if protoRoutes["disabled-model"] == nil || protoRoutes["disabled-model"].Providers == nil {
		t.Fatalf("empty route did not remain an explicit empty provider list: %+v", protoRoutes["disabled-model"])
	}
	converted := providerRoutesFromProto(map[string]*pb.ProviderList{"disabled-model": {}})
	if providers, ok := converted["disabled-model"]; !ok || providers == nil || len(providers) != 0 {
		t.Fatalf("disabled route round trip = %#v", converted)
	}
}

func TestProviderConfigConversionRejectsMaxEventBytesOutsideInt32(t *testing.T) {
	for _, value := range []int64{-1, 1 << 31} {
		_, err := providerConfigToProto(ProviderConfig{Timeouts: &ProviderTimeouts{MaxEventBytes: value}})
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("max_event_bytes=%d error=%v, want ErrInvalidArgument", value, err)
		}
	}
}
