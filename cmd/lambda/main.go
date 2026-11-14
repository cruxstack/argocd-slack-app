// Command lambda runs argocd-slack-app on AWS Lambda: scheduled polls of
// each cluster's Argo CD, the daily digest, and the Slack app's requests.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	_ "time/tzdata"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awslambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/cruxstack/argocd-slack-app/internal/argocdslack"
	"github.com/cruxstack/argocd-slack-app/internal/kube"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})).With("version", version)

	ctx := context.Background()
	cfg, err := argocdslack.LoadConfig(os.Getenv("ARGOCD_SLACK_CONFIG"))
	if err != nil {
		log.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	// icons come from this release's tag unless the config points elsewhere
	if strings.HasPrefix(version, "v") && cfg.IconBaseURL == argocdslack.DefaultIconBaseURL("main") {
		cfg.IconBaseURL = argocdslack.DefaultIconBaseURL(version)
	}
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Error("failed to load aws config", "error", err)
		os.Exit(1)
	}

	sec := &secrets{ssm: ssm.NewFromConfig(awsCfg), log: log, kmsKey: os.Getenv("SSM_KMS_KEY_ID")}
	slackClient := &argocdslack.SlackClient{DryRun: cfg.DryRun, Log: log}
	lam := awslambda.NewFromConfig(awsCfg)
	self := os.Getenv("AWS_LAMBDA_FUNCTION_NAME")

	app := &argocdslack.App{
		Cfg:   cfg,
		Store: &argocdslack.Dynamo{Client: dynamodb.NewFromConfig(awsCfg), Table: os.Getenv("TABLE_NAME")},
		Slack: slackClient,
		Log:   log,
		Connect: func(ctx context.Context, cl argocdslack.Cluster) (argocdslack.ClusterAPI, error) {
			return kube.Connect(ctx, awsCfg, kube.Target{Cluster: cl.Cluster, Region: cl.Region, RoleARN: cl.RoleARN}, "argocd-slack-app")
		},
		Invoke: func(ctx context.Context, job argocdslack.Job) error {
			payload, _ := json.Marshal(job)
			_, err := lam.Invoke(ctx, &awslambda.InvokeInput{
				FunctionName:   aws.String(self),
				InvocationType: types.InvocationTypeEvent,
				Payload:        payload,
			})
			return err
		},
	}

	app.Secrets, app.SetupAPI = sec, slackClient

	lambda.Start(func(ctx context.Context, raw json.RawMessage) (any, error) {
		slackClient.Token = sec.Get(ctx, argocdslack.SecretBotToken)
		requestID := "local"
		if lc, ok := lambdacontext.FromContext(ctx); ok {
			requestID = lc.AwsRequestID
		}
		return app.Handle(ctx, raw, requestID)
	})
}

// secrets reads and writes the SSM parameters named by environment variables
// (each holds a parameter ARN), caching values found once per container.
type secrets struct {
	ssm    *ssm.Client
	log    *slog.Logger
	kmsKey string
	mu     sync.Mutex
	vals   map[string]string
}

func parameterName(env string) string {
	ref := os.Getenv(env)
	if i := strings.Index(ref, ":parameter"); strings.HasPrefix(ref, "arn:") && i > 0 {
		return ref[i+len(":parameter"):]
	}
	return ref
}

func (s *secrets) Get(ctx context.Context, env string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.vals[env]; ok {
		return v
	}
	name := parameterName(env)
	if name == "" {
		return ""
	}
	out, err := s.ssm.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name), WithDecryption: aws.Bool(true)})
	if err != nil {
		var nf *ssmtypes.ParameterNotFound
		if !errors.As(err, &nf) {
			s.log.WarnContext(ctx, "failed to read parameter", "env", env, "error", err)
		}
		return ""
	}
	if s.vals == nil {
		s.vals = map[string]string{}
	}
	s.vals[env] = aws.ToString(out.Parameter.Value)
	return s.vals[env]
}

func (s *secrets) Put(ctx context.Context, env, value string, secure bool) error {
	name := parameterName(env)
	if name == "" {
		return fmt.Errorf("%s is not set", env)
	}
	in := &ssm.PutParameterInput{Name: aws.String(name), Value: aws.String(value), Overwrite: aws.Bool(true), Type: ssmtypes.ParameterTypeString}
	if secure {
		in.Type = ssmtypes.ParameterTypeSecureString
		if s.kmsKey != "" {
			in.KeyId = aws.String(s.kmsKey)
		}
	}
	if _, err := s.ssm.PutParameter(ctx, in); err != nil {
		return fmt.Errorf("failed to write %s: %w", name, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vals == nil {
		s.vals = map[string]string{}
	}
	s.vals[env] = value
	return nil
}
