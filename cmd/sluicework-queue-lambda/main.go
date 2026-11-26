// This function is invoked by an EventBridge clock, never by a public URL.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"sluicework/internal/awsspot"
	"sluicework/internal/githubapp"
	"sluicework/internal/githubqueue"
)

func initialize(ctx context.Context) (*githubqueue.Engine, error) {
	var cfg githubqueue.Config
	if err := json.Unmarshal([]byte(os.Getenv("QUEUE_CONFIG")), &cfg); err != nil {
		return nil, errors.New("invalid queue configuration")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	aws, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, errors.New("cannot load AWS execution identity")
	}
	secretARN := os.Getenv("GITHUB_KEY_SECRET_ARN")
	if secretARN == "" || os.Getenv("QUEUE_TABLE") == "" {
		return nil, errors.New("configure the queue table and GitHub key secret")
	}
	secret, err := secretsmanager.NewFromConfig(aws).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &secretARN})
	if err != nil {
		return nil, errors.New("cannot read GitHub App key from the configured secret")
	}
	key := secret.SecretBinary
	if secret.SecretString != nil {
		key = []byte(*secret.SecretString)
	}
	store := &githubqueue.DynamoStore{Client: dynamodb.NewFromConfig(aws), Table: os.Getenv("QUEUE_TABLE")}
	engine, err := githubqueue.NewEngine(cfg, key, store)
	if err != nil {
		return nil, err
	}
	if cfg.Spot != nil {
		repos := []string{}
		sha := ""
		for _, r := range cfg.Repositories {
			repos = append(repos, r.Name)
			if r.Name == cfg.Spot.ControlRepository {
				sha = r.HeartbeatSHA
			}
		}
		// EC2 may run in a different region; keep queue state and secrets local.
		runnerAWS := aws.Copy()
		runnerAWS.Region = cfg.Spot.Region
		fleet, e := awsspot.NewFleet(ctx, *cfg.Spot, cfg.BackendID, repos, githubapp.Config{AppID: cfg.AppID, InstallationID: cfg.InstallationID}, key, runnerAWS, &awsspot.DynamoStore{Client: store.Client, Table: store.Table, BackendID: cfg.BackendID})
		if e != nil {
			return nil, e
		}
		fleet.HeartbeatSHA = sha
		engine.Fleet = fleet
	}
	return engine, nil
}
func main() {
	var engine *githubqueue.Engine
	var refreshed time.Time
	var gate clockGate
	lambda.Start(func(ctx context.Context, event json.RawMessage) error {
		return gate.run(event, time.Now(), func() error {
			if engine == nil || time.Since(refreshed) > 15*time.Minute {
				var err error
				engine, err = initialize(ctx)
				if err != nil {
					log.Print(err)
					return err
				}
				refreshed = time.Now()
			}
			return engine.Tick(ctx)
		})
	})
}
