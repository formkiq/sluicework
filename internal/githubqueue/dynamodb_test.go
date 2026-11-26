package githubqueue

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"testing"
	"time"
)

type dynamoMock struct {
	put   func(*dynamodb.PutItemInput) error
	get   func(*dynamodb.GetItemInput) *dynamodb.GetItemOutput
	query func(*dynamodb.QueryInput) *dynamodb.QueryOutput
}

func (m dynamoMock) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	return &dynamodb.PutItemOutput{}, m.put(in)
}
func (m dynamoMock) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return m.get(in), nil
}
func (m dynamoMock) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	return m.query(in), nil
}
func (m dynamoMock) DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	return &dynamodb.DeleteItemOutput{}, nil
}
func TestDynamoQueueConditionalSaveAndTerminalIndex(t *testing.T) {
	ctx := context.Background()
	r := request("one", High, time.Now())
	r.RepositoryID = 7
	r.Kind = "site"
	store := &DynamoStore{Table: "test"}
	calls := 0
	store.Client = dynamoMock{put: func(in *dynamodb.PutItemInput) error {
		calls++
		if *in.TableName != "test" {
			t.Fatal("table scope changed")
		}
		if calls == 1 {
			if *in.ConditionExpression != "attribute_not_exists(id)" || in.Item["active"] == nil {
				t.Fatal("missing create condition or active index")
			}
		} else {
			if in.ExpressionAttributeNames["#version"] != "version" || in.ExpressionAttributeValues[":version"] == nil {
				t.Fatal("missing CAS")
			}
			if in.Item["active"] != nil {
				t.Fatal("terminal record remained indexed")
			}
		}
		return nil
	}}
	if err := store.Save(ctx, &r); err != nil || r.Version != 1 {
		t.Fatal(r, err)
	}
	r.State = "completed"
	r.CallbackSent = true
	if err := store.Save(ctx, &r); err != nil || r.Version != 2 {
		t.Fatal(r, err)
	}
	store.Client = dynamoMock{put: func(*dynamodb.PutItemInput) error { return &types.ConditionalCheckFailedException{} }}
	if err := store.Save(ctx, &r); !errors.Is(err, ErrConflict) || r.Version != 2 {
		t.Fatal(r, err)
	}
	r.Version = 0
	if err := store.Save(ctx, &r); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
}
func TestDynamoActiveIndexAlwaysUsesStrongRecordRead(t *testing.T) {
	r := request("one", High, time.Now())
	r.RepositoryID = 7
	r.Kind = "site"
	r.Version = 4
	r.State = "completed"
	r.CallbackSent = true
	b, _ := json.Marshal(r)
	reads := 0
	client := dynamoMock{query: func(in *dynamodb.QueryInput) *dynamodb.QueryOutput {
		if *in.IndexName != "active-requests" {
			t.Fatal("unexpected index")
		}
		return &dynamodb.QueryOutput{Items: []map[string]types.AttributeValue{{"id": textAttribute("request:" + r.Key())}}}
	}, get: func(in *dynamodb.GetItemInput) *dynamodb.GetItemOutput {
		reads++
		if in.ConsistentRead == nil || !*in.ConsistentRead {
			t.Fatal("eventually consistent state can repeat side effects")
		}
		return &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{"body": textAttribute(string(b))}}
	}}
	store := &DynamoStore{Client: client, Table: "test"}
	active, err := store.Active(context.Background())
	if err != nil || len(active) != 0 || reads != 1 {
		t.Fatal(active, reads, err)
	}
}
func TestDynamoLeaseIsConditionalAndUsesMilliseconds(t *testing.T) {
	now := time.Unix(1000, 0)
	store := &DynamoStore{Client: dynamoMock{put: func(in *dynamodb.PutItemInput) error {
		if in.Item["expires"].(*types.AttributeValueMemberN).Value != "1120000" || in.ConditionExpression == nil {
			t.Fatal("lease deadline or condition missing")
		}
		return &types.ConditionalCheckFailedException{}
	}}, Table: "test"}
	ok, err := store.Lease(context.Background(), "owner", now, 2*time.Minute)
	if ok || err != nil {
		t.Fatal(ok, err)
	}
}
