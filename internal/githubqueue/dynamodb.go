package githubqueue

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type DynamoAPI interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}
type DynamoStore struct {
	Client DynamoAPI
	Table  string
}

func pointer(s string) *string                    { return &s }
func textAttribute(s string) types.AttributeValue { return &types.AttributeValueMemberS{Value: s} }
func numberAttribute(n int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(n, 10)}
}
func conditional(err error) bool {
	var rejected *types.ConditionalCheckFailedException
	return errors.As(err, &rejected)
}
func (s *DynamoStore) Lease(ctx context.Context, owner string, now time.Time, duration time.Duration) (bool, error) {
	_, err := s.Client.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.Table, Item: map[string]types.AttributeValue{"id": textAttribute("lease"), "owner": textAttribute(owner), "expires": numberAttribute(now.Add(duration).UnixMilli())}, ConditionExpression: pointer("attribute_not_exists(id) OR expires <= :now"), ExpressionAttributeValues: map[string]types.AttributeValue{":now": numberAttribute(now.UnixMilli())}})
	if conditional(err) {
		return false, nil
	}
	return err == nil, err
}
func (s *DynamoStore) Release(ctx context.Context, owner string) error {
	_, err := s.Client.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: &s.Table, Key: map[string]types.AttributeValue{"id": textAttribute("lease")}, ConditionExpression: pointer("#owner = :owner"), ExpressionAttributeNames: map[string]string{"#owner": "owner"}, ExpressionAttributeValues: map[string]types.AttributeValue{":owner": textAttribute(owner)}})
	if conditional(err) {
		return nil
	}
	return err
}
func (s *DynamoStore) Get(ctx context.Context, key string) (Request, error) {
	var r Request
	yes := true
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{TableName: &s.Table, ConsistentRead: &yes, Key: map[string]types.AttributeValue{"id": textAttribute("request:" + key)}})
	if err != nil {
		return r, err
	}
	if len(out.Item) == 0 {
		return r, ErrNotFound
	}
	body, ok := out.Item["body"].(*types.AttributeValueMemberS)
	if !ok {
		return r, errors.New("invalid persisted queue record")
	}
	err = json.Unmarshal([]byte(body.Value), &r)
	return r, err
}
func (s *DynamoStore) Save(ctx context.Context, r *Request) error {
	next := *r
	next.Version++
	body, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if len(body) > 300000 {
		return errors.New("queue record exceeds supported size")
	}
	item := map[string]types.AttributeValue{"id": textAttribute("request:" + r.Key()), "version": numberAttribute(next.Version), "body": textAttribute(string(body)), "expiresAt": numberAttribute(time.Now().Add(7 * 24 * time.Hour).Unix())}
	if r.Active() {
		item["active"] = textAttribute("active")
		item["created"] = textAttribute(r.Created.UTC().Format(time.RFC3339Nano) + ":" + r.Key())
	}
	condition := "attribute_not_exists(id)"
	var values map[string]types.AttributeValue
	if r.Version > 0 {
		condition = "#version = :version"
		values = map[string]types.AttributeValue{":version": numberAttribute(r.Version)}
	}
	input := &dynamodb.PutItemInput{TableName: &s.Table, Item: item, ConditionExpression: &condition, ExpressionAttributeValues: values}
	if r.Version > 0 {
		input.ExpressionAttributeNames = map[string]string{"#version": "version"}
	}
	_, err = s.Client.PutItem(ctx, input)
	if conditional(err) {
		if r.Version == 0 {
			return ErrExists
		}
		return ErrConflict
	}
	if err == nil {
		r.Version = next.Version
	}
	return err
}
func (s *DynamoStore) Active(ctx context.Context) ([]Request, error) {
	result := []Request{}
	var start map[string]types.AttributeValue
	for {
		out, err := s.Client.Query(ctx, &dynamodb.QueryInput{TableName: &s.Table, IndexName: pointer("active-requests"), KeyConditionExpression: pointer("#active = :active"), ExpressionAttributeNames: map[string]string{"#active": "active"}, ExpressionAttributeValues: map[string]types.AttributeValue{":active": textAttribute("active")}, ExclusiveStartKey: start})
		if err != nil {
			return nil, err
		}
		// GSI reads are eventually consistent. Re-read each record strongly before
		// applying its optimistic version or making a side effect.
		for _, item := range out.Items {
			id, ok := item["id"].(*types.AttributeValueMemberS)
			if !ok || !strings.HasPrefix(id.Value, "request:") {
				return nil, errors.New("invalid active queue key")
			}
			r, err := s.Get(ctx, id.Value[8:])
			if err != nil {
				return nil, err
			}
			if r.Active() {
				result = append(result, r)
			}
		}
		if len(result) > 1000 {
			return nil, errors.New("too many active requests")
		}
		if len(out.LastEvaluatedKey) == 0 {
			return result, nil
		}
		start = out.LastEvaluatedKey
	}
}

var _ Store = (*DynamoStore)(nil)
