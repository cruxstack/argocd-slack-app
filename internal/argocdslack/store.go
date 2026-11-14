package argocdslack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ErrConflict means someone else saved the item first.
var ErrConflict = errors.New("item changed since it was read")

// Store keeps app state, mutes and bookkeeping.
type Store interface {
	LoadCluster(ctx context.Context, cluster string) (map[string]*AppRecord, *ClusterMeta, error)
	GetRecord(ctx context.Context, cluster, app string) (*AppRecord, error)
	SaveRecord(ctx context.Context, rec *AppRecord) error
	DeleteRecord(ctx context.Context, cluster, app string) error
	SaveMeta(ctx context.Context, m *ClusterMeta) error
	Mutes(ctx context.Context) ([]Mute, error)
	PutMute(ctx context.Context, m Mute) error
	DeleteMute(ctx context.Context, id string) error
	AcquireLease(ctx context.Context, owner string, ttl time.Duration, now time.Time) (bool, error)
	ReleaseLease(ctx context.Context, owner string) error
	MarkOnce(ctx context.Context, key string, ttl time.Duration, now time.Time) (bool, error)
	TryLock(ctx context.Context, name string, ttl time.Duration, now time.Time) (bool, error)
	Unlock(ctx context.Context, name string) error
}

// Dynamo is the DynamoDB Store; pk/sk strings, a JSON doc and a version.
type Dynamo struct {
	Client *dynamodb.Client
	Table  string
}

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func n(v int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}

func key(pk, sk string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": s(pk), "sk": s(sk)}
}

func attrS(item map[string]types.AttributeValue, k string) string {
	if v, ok := item[k].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func attrN(item map[string]types.AttributeValue, k string) int64 {
	if v, ok := item[k].(*types.AttributeValueMemberN); ok {
		i, _ := strconv.ParseInt(v.Value, 10, 64)
		return i
	}
	return 0
}

func clusterPK(c string) string { return "CLUSTER#" + c }

func (d *Dynamo) LoadCluster(ctx context.Context, cluster string) (map[string]*AppRecord, *ClusterMeta, error) {
	recs := map[string]*AppRecord{}
	meta := &ClusterMeta{Cluster: cluster}
	p := dynamodb.NewQueryPaginator(d.Client, &dynamodb.QueryInput{
		TableName:                 aws.String(d.Table),
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(clusterPK(cluster))},
		ConsistentRead:            aws.Bool(true),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("load %s: %w", cluster, err)
		}
		for _, item := range page.Items {
			sk, doc, ver := attrS(item, "sk"), attrS(item, "doc"), attrN(item, "ver")
			switch {
			case sk == "META":
				if err := json.Unmarshal([]byte(doc), meta); err != nil {
					return nil, nil, fmt.Errorf("meta %s: %w", cluster, err)
				}
				meta.Version = ver
			case strings.HasPrefix(sk, "APP#"):
				rec := &AppRecord{}
				if err := json.Unmarshal([]byte(doc), rec); err != nil {
					return nil, nil, fmt.Errorf("record %s: %w", sk, err)
				}
				rec.Version = ver
				recs[rec.App] = rec
			}
		}
	}
	return recs, meta, nil
}

func (d *Dynamo) GetRecord(ctx context.Context, cluster, app string) (*AppRecord, error) {
	out, err := d.Client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(d.Table), Key: key(clusterPK(cluster), "APP#"+app), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return nil, err
	}
	if out.Item == nil {
		return nil, nil
	}
	rec := &AppRecord{}
	if err := json.Unmarshal([]byte(attrS(out.Item, "doc")), rec); err != nil {
		return nil, err
	}
	rec.Version = attrN(out.Item, "ver")
	return rec, nil
}

func (d *Dynamo) putVersioned(ctx context.Context, pk, sk string, doc any, ver *int64, extra map[string]types.AttributeValue) error {
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	item := key(pk, sk)
	item["doc"], item["ver"] = s(string(b)), n(*ver+1)
	for k, v := range extra {
		item[k] = v
	}
	in := &dynamodb.PutItemInput{TableName: aws.String(d.Table), Item: item}
	if *ver == 0 {
		in.ConditionExpression = aws.String("attribute_not_exists(pk)")
	} else {
		in.ConditionExpression = aws.String("ver = :v")
		in.ExpressionAttributeValues = map[string]types.AttributeValue{":v": n(*ver)}
	}
	if _, err := d.Client.PutItem(ctx, in); err != nil {
		var cf *types.ConditionalCheckFailedException
		if errors.As(err, &cf) {
			return ErrConflict
		}
		return err
	}
	*ver++
	return nil
}

func (d *Dynamo) SaveRecord(ctx context.Context, rec *AppRecord) error {
	return d.putVersioned(ctx, clusterPK(rec.Cluster), "APP#"+rec.App, rec, &rec.Version, nil)
}

func (d *Dynamo) DeleteRecord(ctx context.Context, cluster, app string) error {
	_, err := d.Client.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: aws.String(d.Table), Key: key(clusterPK(cluster), "APP#"+app)})
	return err
}

func (d *Dynamo) SaveMeta(ctx context.Context, m *ClusterMeta) error {
	return d.putVersioned(ctx, clusterPK(m.Cluster), "META", m, &m.Version, nil)
}

func (d *Dynamo) Mutes(ctx context.Context) ([]Mute, error) {
	out, err := d.Client.Query(ctx, &dynamodb.QueryInput{
		TableName:                 aws.String(d.Table),
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s("MUTE")},
		ConsistentRead:            aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	var mutes []Mute
	for _, item := range out.Items {
		var m Mute
		if json.Unmarshal([]byte(attrS(item, "doc")), &m) == nil {
			mutes = append(mutes, m)
		}
	}
	return mutes, nil
}

func (d *Dynamo) PutMute(ctx context.Context, m Mute) error {
	b, _ := json.Marshal(m)
	item := key("MUTE", m.ID)
	item["doc"] = s(string(b))
	item["ttl"] = n(m.Until.Add(24 * time.Hour).Unix())
	_, err := d.Client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(d.Table), Item: item})
	return err
}

func (d *Dynamo) DeleteMute(ctx context.Context, id string) error {
	_, err := d.Client.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: aws.String(d.Table), Key: key("MUTE", id)})
	return err
}

func (d *Dynamo) AcquireLease(ctx context.Context, owner string, ttl time.Duration, now time.Time) (bool, error) {
	item := key("LEASE", "POLLER")
	item["owner"], item["expires"], item["ttl"] = s(owner), n(now.Add(ttl).Unix()), n(now.Add(ttl+time.Hour).Unix())
	_, err := d.Client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                 aws.String(d.Table),
		Item:                      item,
		ConditionExpression:       aws.String("attribute_not_exists(pk) OR expires < :now OR #o = :me"),
		ExpressionAttributeNames:  map[string]string{"#o": "owner"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":now": n(now.Unix()), ":me": s(owner)},
	})
	var cf *types.ConditionalCheckFailedException
	if errors.As(err, &cf) {
		return false, nil
	}
	return err == nil, err
}

func (d *Dynamo) ReleaseLease(ctx context.Context, owner string) error {
	_, err := d.Client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:                 aws.String(d.Table),
		Key:                       key("LEASE", "POLLER"),
		ConditionExpression:       aws.String("#o = :me"),
		ExpressionAttributeNames:  map[string]string{"#o": "owner"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":me": s(owner)},
	})
	var cf *types.ConditionalCheckFailedException
	if errors.As(err, &cf) {
		return nil
	}
	return err
}

// MarkOnce returns true the first time key is marked.
func (d *Dynamo) MarkOnce(ctx context.Context, k string, ttl time.Duration, now time.Time) (bool, error) {
	item := key("ONCE", k)
	item["ttl"] = n(now.Add(ttl).Unix())
	_, err := d.Client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(d.Table),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(pk)"),
	})
	var cf *types.ConditionalCheckFailedException
	if errors.As(err, &cf) {
		return false, nil
	}
	return err == nil, err
}

// TryLock takes a named lock that expires after ttl, so a crash never leaves
// it held for long.
func (d *Dynamo) TryLock(ctx context.Context, name string, ttl time.Duration, now time.Time) (bool, error) {
	item := key("LOCK", name)
	item["expires"], item["ttl"] = n(now.Add(ttl).Unix()), n(now.Add(ttl+time.Hour).Unix())
	_, err := d.Client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                 aws.String(d.Table),
		Item:                      item,
		ConditionExpression:       aws.String("attribute_not_exists(pk) OR expires < :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":now": n(now.Unix())},
	})
	var cf *types.ConditionalCheckFailedException
	if errors.As(err, &cf) {
		return false, nil
	}
	return err == nil, err
}

// Unlock releases a named lock.
func (d *Dynamo) Unlock(ctx context.Context, name string) error {
	_, err := d.Client.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: aws.String(d.Table), Key: key("LOCK", name)})
	return err
}
