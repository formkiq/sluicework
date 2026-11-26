package awsspot

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"sluicework/internal/spot"
	"testing"
)

type fakeDynamo struct {
	item       map[string]types.AttributeValue
	consistent bool
	conflict   bool
}

func (f *fakeDynamo) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.consistent = in.ConsistentRead != nil && *in.ConsistentRead
	return &dynamodb.GetItemOutput{Item: f.item}, nil
}
func (f *fakeDynamo) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	if f.conflict {
		return nil, &types.ConditionalCheckFailedException{}
	}
	if in.ConditionExpression == nil {
		return nil, errors.New("missing atomic condition")
	}
	f.item = in.Item
	return &dynamodb.PutItemOutput{}, nil
}
func TestFleetDynamoUsesStrongAtomicPrivateRecordWithoutTTL(t *testing.T) {
	f := &fakeDynamo{}
	s := &DynamoStore{Client: f, Table: "queue", BackendID: "test"}
	ctx := context.Background()
	state, e := s.Load(ctx)
	if e != nil || !f.consistent {
		t.Fatal(e)
	}
	state.Allocations["one"] = spot.Allocation{State: "launching", LaunchUncertain: true}
	if e = s.Save(ctx, &state); e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"active", "created", "expiresAt"} {
		if _, ok := f.item[key]; ok {
			t.Fatal("private fleet exposed through GSI or TTL", key)
		}
	}
	saved, e := s.Load(ctx)
	if e != nil || saved.Version != 1 || !saved.Allocations["one"].LaunchUncertain {
		t.Fatal(e, saved)
	}
	f.conflict = true
	if e = s.Save(ctx, &saved); !errors.Is(e, spot.ErrConflict) {
		t.Fatal(e)
	}
}
