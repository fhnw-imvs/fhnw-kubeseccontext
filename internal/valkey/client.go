package valkey

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	checksv1alpha1 "github.com/fhnw-imvs/fhnw-kubeseccontext/api/v1alpha1"
	"github.com/valkey-io/valkey-go"
)

type ValkeyClient struct {
	valkey.Client
}

func NewValKeyClient(valkeyHost, valkeyPort string) (*ValkeyClient, error) {

	client, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{valkeyHost + ":" + valkeyPort}})
	if err != nil {
		return nil, err
	}

	// Ensure the client is connected
	if err = client.Do(context.TODO(), client.B().Ping().Build()).Error(); err != nil {
		return nil, err
	}

	return &ValkeyClient{client}, nil
}

func (v *ValkeyClient) storeEntry(ctx context.Context, key string, value string) error {

	// Store the entry with a key and value, setting an expiration time of 3600 seconds (1 hour)
	err := v.Do(ctx, v.B().Set().Key(strings.ToLower(key)).Value(value).Ex(3600*time.Second).Build()).Error()
	if err != nil {
		return err
	}

	return nil
}

func (v *ValkeyClient) getEntry(ctx context.Context, key string) (string, error) {

	res, err := v.Do(ctx, v.B().Get().Key(strings.ToLower(key)).Build()).AsStrSlice()
	if err != nil {
		return "", err
	}

	if len(res) == 0 {
		return "", nil // No entry found for the given key
	}
	return res[0], nil
}

func (v ValkeyClient) deleteEntry(ctx context.Context, key string) error {

	err := v.Do(ctx, v.B().Del().Key(strings.ToLower(key)).Build()).Error()
	if err != nil {
		return err
	}

	return nil
}

func (v ValkeyClient) StoreRecording(ctx context.Context, suffix string, recording checksv1alpha1.WorkloadRecording) error {
	key := "recording:" + suffix + ":" + recording.Type

	// Store the metrics in Valkey
	jsonB, err := json.Marshal(recording)
	if err != nil {
		return err
	}

	return v.storeEntry(ctx, key, string(jsonB))
}

func (v ValkeyClient) GetRecording(ctx context.Context, keySuffix string) (*checksv1alpha1.WorkloadRecording, error) {

	key := "recording:" + keySuffix

	value, err := v.getEntry(ctx, key)
	if err != nil {
		return nil, err
	}
	if value == "" {
		return nil, nil // No recording found for the given key
	}

	var recording checksv1alpha1.WorkloadRecording
	err = json.Unmarshal([]byte(value), &recording)
	if err != nil {
		return nil, err
	}
	return &recording, nil

}

func (v ValkeyClient) DeleteRecording(ctx context.Context, keySuffix, recordingType string) error {
	key := "recording:" + keySuffix + ":" + recordingType

	return v.deleteEntry(ctx, key)
}
