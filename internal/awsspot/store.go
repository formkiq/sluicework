package awsspot

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"sluicework/internal/spot"
	"strconv"
)

type DynamoAPI interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
}
type DynamoStore struct {
	Client           DynamoAPI
	Table, BackendID string
}

func (s *DynamoStore) key() map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: "spot-fleet:" + s.BackendID}}
}
func (s *DynamoStore) Load(ctx context.Context) (spot.State, error) {
	yes := true
	out, e := s.Client.GetItem(ctx, &dynamodb.GetItemInput{TableName: &s.Table, Key: s.key(), ConsistentRead: &yes})
	if e != nil {
		return spot.State{}, errors.New("cannot read the private Spot fleet state")
	}
	state := spot.State{Allocations: map[string]spot.Allocation{}}
	if len(out.Item) == 0 {
		return state, nil
	}
	body, ok := out.Item["body"].(*types.AttributeValueMemberS)
	if !ok || len(body.Value) > 300000 || json.Unmarshal([]byte(body.Value), &state) != nil || len(state.Allocations) > spot.HardLimit {
		return spot.State{}, errors.New("invalid persisted Spot fleet")
	}
	v, ok := out.Item["version"].(*types.AttributeValueMemberN)
	if !ok || strconv.FormatInt(state.Version, 10) != v.Value {
		return spot.State{}, errors.New("invalid Spot fleet version")
	}
	return state, nil
}
func (s *DynamoStore) Save(ctx context.Context, state *spot.State) error {
	if len(state.Allocations) > spot.HardLimit {
		return errors.New("Spot fleet exceeds its hard limit")
	}
	next := *state
	next.Version++
	body, e := json.Marshal(next)
	if e != nil {
		return e
	}
	if len(body) > 300000 {
		return errors.New("private Spot fleet exceeds its storage limit")
	}
	item := s.key()
	item["version"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(next.Version, 10)}
	item["body"] = &types.AttributeValueMemberS{Value: string(body)}
	condition := "attribute_not_exists(id)"
	names := map[string]string(nil)
	values := map[string]types.AttributeValue(nil)
	if state.Version > 0 {
		condition = "#version = :version"
		names = map[string]string{"#version": "version"}
		values = map[string]types.AttributeValue{":version": &types.AttributeValueMemberN{Value: strconv.FormatInt(state.Version, 10)}}
	}
	// No expiry attribute: an occupied slot must never disappear through TTL.
	_, e = s.Client.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.Table, Item: item, ConditionExpression: &condition, ExpressionAttributeNames: names, ExpressionAttributeValues: values})
	var conflict *types.ConditionalCheckFailedException
	if errors.As(e, &conflict) {
		return spot.ErrConflict
	}
	if e != nil {
		return errors.New("cannot persist the private Spot fleet state")
	}
	state.Version = next.Version
	return nil
}

var _ spot.Store = (*DynamoStore)(nil)
