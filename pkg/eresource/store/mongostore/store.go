package mongostore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zzjha-cn/gKit/pkg/eresource/model"
	"github.com/zzjha-cn/gKit/pkg/eresource/store"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type MongoStore struct {
	client     *mongo.Client
	database   string
	collection string
}

func NewMongoStore(client *mongo.Client, database, collection string) (*MongoStore, error) {
	if client == nil {
		return nil, fmt.Errorf("mongo client is nil")
	}
	if database == "" {
		return nil, fmt.Errorf("mongo database is empty")
	}
	if collection == "" {
		collection = "eresource"
	}
	return &MongoStore{client: client, database: database, collection: collection}, nil
}

func (s *MongoStore) Create(ctx context.Context, resource *model.Resource) error {
	now := time.Now()
	if resource.CreatedAt.IsZero() {
		resource.CreatedAt = now
	}
	if resource.UpdatedAt.IsZero() {
		resource.UpdatedAt = now
	}
	_, err := s.col().InsertOne(ctx, resource)
	if mongo.IsDuplicateKeyError(err) {
		// 幂等键撞车是预期内的并发结果，不是故障：调用方据此回查一次
		// 已有记录返回即可，所以必须能和真正的写入失败区分开。
		return fmt.Errorf("%w: %v", store.ErrDuplicate, err)
	}
	return err
}

// Update 只更新可变字段。
// 直接 $set 整个结构体会把 id、created_at 一并覆盖，且零值字段会抹掉已有数据。
func (s *MongoStore) Update(ctx context.Context, resource *model.Resource) error {
	resource.UpdatedAt = time.Now()
	update := bson.M{"$set": bson.M{
		"namespace":             resource.Namespace,
		"account_id":            resource.AccountID,
		"name":                  resource.Name,
		"ext":                   resource.Ext,
		"mime_type":             resource.MimeType,
		"size":                  resource.Size,
		"hash":                  resource.Hash,
		"provider":              resource.Provider,
		"bucket":                resource.Bucket,
		"object_key":            resource.ObjectKey,
		"store_path":            resource.StorePath,
		"url":                   resource.URL,
		"scheme":                resource.Scheme,
		"status":                resource.Status,
		"biz_type":              resource.BizType,
		"tags":                  resource.Tags,
		"access_policy":         resource.AccessPolicy,
		"hotlink_protection":    resource.HotlinkProtection,
		"signed_url_expire_at":  resource.SignedURLExpireAt,
		"metadata":              resource.Metadata,
		"shared":                resource.Shared,
		"idempotency_expire_at": resource.IdempotencyExpireAt,
		"version":               resource.Version,
		"parent_id":             resource.ParentID,
		"updated_at":            resource.UpdatedAt,
	}}
	result, err := s.col().UpdateOne(ctx, bson.M{"id": resource.ID}, update)
	if err != nil {
		return err
	}
	if result.MatchedCount == 0 {
		return store.ErrNotFound
	}
	return nil
}

// Patch 按补丁做局部 $set，未设置的字段不进入更新语句。
func (s *MongoStore) Patch(ctx context.Context, id string, patch *model.ResourcePatch) error {
	if patch == nil || patch.IsEmpty() {
		return nil
	}
	fields := bson.M{}
	if patch.Name != nil {
		fields["name"] = *patch.Name
	}
	if patch.BizType != nil {
		fields["biz_type"] = *patch.BizType
	}
	if patch.AccessPolicy != nil {
		fields["access_policy"] = *patch.AccessPolicy
	}
	if patch.DeletedAt != nil {
		fields["deleted_at"] = *patch.DeletedAt
	}
	if patch.Status != nil {
		fields["status"] = *patch.Status
	}
	if patch.URL != nil {
		fields["url"] = *patch.URL
	}
	if patch.Tags != nil {
		fields["tags"] = patch.Tags
	}
	if patch.Metadata != nil {
		fields["metadata"] = patch.Metadata
	}
	updatedAt := patch.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	fields["updated_at"] = updatedAt
	update := bson.M{"$set": fields}
	if patch.ClearIdempotencyKey {
		update["$unset"] = bson.M{"idempotency_key": "", "idempotency_expire_at": ""}
	}
	result, err := s.col().UpdateOne(ctx, bson.M{"id": id}, update)
	if err != nil {
		return err
	}
	if result.MatchedCount == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *MongoStore) Delete(ctx context.Context, id string) error {
	result, err := s.col().DeleteOne(ctx, bson.M{"id": id})
	if err != nil {
		return err
	}
	if result.DeletedCount == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *MongoStore) GetByID(ctx context.Context, id string) (*model.Resource, error) {
	var resource model.Resource
	err := s.col().FindOne(ctx, bson.M{"id": id}).Decode(&resource)
	if err != nil {
		return nil, wrapNotFound(err)
	}
	return &resource, nil
}

func (s *MongoStore) GetByHash(ctx context.Context, hash string) (*model.Resource, error) {
	var resource model.Resource
	filter := bson.M{"hash": hash, "status": model.ResourceStatusActive}
	// 命中多条时取最新的一条，结果稳定可预期
	opt := options.FindOne().SetSort(bson.D{{Key: "created_at", Value: -1}})
	err := s.col().FindOne(ctx, filter, opt).Decode(&resource)
	if err != nil {
		return nil, wrapNotFound(err)
	}
	return &resource, nil
}

func (s *MongoStore) List(ctx context.Context, query *model.Query) ([]*model.Resource, error) {
	filter := s.buildFilter(query)
	// 与 JSONStore 行为对齐：按创建时间倒序，保证分页结果稳定
	findOptions := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}})
	if query != nil {
		if query.Offset > 0 {
			findOptions.SetSkip(query.Offset)
		}
		if query.Limit > 0 {
			findOptions.SetLimit(query.Limit)
		}
	}
	cursor, err := s.col().Find(ctx, filter, findOptions)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var resources []*model.Resource
	for cursor.Next(ctx) {
		var resource model.Resource
		if err = cursor.Decode(&resource); err != nil {
			return nil, err
		}
		resources = append(resources, &resource)
	}
	if err = cursor.Err(); err != nil {
		return nil, err
	}
	return resources, nil
}

func (s *MongoStore) Count(ctx context.Context, query *model.Query) (int64, error) {
	return s.col().CountDocuments(ctx, s.buildFilter(query))
}

func (s *MongoStore) EnsureIndexes(ctx context.Context) error {
	indexes := []mongo.IndexModel{
		{Keys: bson.D{{Key: "id", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "hash", Value: 1}}},
		{Keys: bson.D{{Key: "namespace", Value: 1}, {Key: "account_id", Value: 1}}}, // 引入高频多租户联合索引起步提升检索响应性能
		// 列表查询的实际形态是「租户过滤 + 按创建时间倒序」。只有
		// (namespace, account_id) 与 (created_at) 两条独立索引时，Mongo 能用索引
		// 完成过滤却只能在内存里排序，大租户下会撞上 32MB 的 SORT 上限直接报错。
		{Keys: bson.D{{Key: "namespace", Value: 1}, {Key: "account_id", Value: 1}, {Key: "created_at", Value: -1}}},
		// 去重查询（findDedupTarget）的字段组合。
		{Keys: bson.D{{Key: "namespace", Value: 1}, {Key: "account_id", Value: 1}, {Key: "hash", Value: 1}, {Key: "status", Value: 1}}},
		// 删除前的对象引用计数走这条索引：一个对象被几条记录引用，决定了
		// 能不能真的把它从底层删掉。
		{Keys: bson.D{{Key: "provider", Value: 1}, {Key: "object_key", Value: 1}}},
		// tags 是数组字段，建的是多键索引。
		{Keys: bson.D{{Key: "namespace", Value: 1}, {Key: "account_id", Value: 1}, {Key: "tags", Value: 1}}},
		{Keys: bson.D{{Key: "created_at", Value: -1}}},
		// 幂等键的过期清理按这条索引扫。
		//
		// 刻意**不用 Mongo 的 TTL 索引**：TTL 索引会把整个文档删掉，
		// 而这里要过期的只是键，资源记录必须留着。
		{Keys: bson.D{{Key: "idempotency_expire_at", Value: 1}}, Options: options.Index().SetSparse(true)},
		// 只约束非空字符串键。复合稀疏索引也会索引带有租户字段的无键记录。
		{
			Keys: bson.D{{Key: "idempotency_key", Value: 1}, {Key: "namespace", Value: 1}, {Key: "account_id", Value: 1}},
			Options: options.Index().SetUnique(true).
				SetPartialFilterExpression(bson.M{"idempotency_key": bson.M{"$gt": ""}}).
				SetName("uniq_tenant_nonempty_idempotency_key"),
		},
	}
	indexView := s.col().Indexes()
	if _, err := indexView.CreateMany(ctx, indexes); err != nil {
		return err
	}
	if _, err := indexView.DropOne(ctx, "uniq_tenant_idempotency_key"); err != nil {
		var commandErr mongo.CommandError
		if !errors.As(err, &commandErr) || commandErr.Code != 27 {
			return err
		}
	}
	return nil
}

func (s *MongoStore) col() *mongo.Collection {
	return s.client.Database(s.database).Collection(s.collection)
}

func (s *MongoStore) buildFilter(query *model.Query) bson.M {
	filter := bson.M{}
	if query == nil {
		return filter
	}
	if len(query.IDs) > 0 {
		filter["id"] = bson.M{"$in": query.IDs}
	}
	if query.Namespace != "" {
		filter["namespace"] = query.Namespace
	}
	if query.AccountID != "" {
		filter["account_id"] = query.AccountID
	}
	if query.Hash != "" {
		filter["hash"] = query.Hash
	}
	if query.Provider != "" {
		filter["provider"] = query.Provider
	}
	if query.ObjectKey != "" {
		filter["object_key"] = query.ObjectKey
	}
	if query.BizType != "" {
		filter["biz_type"] = query.BizType
	}
	if query.Status != "" {
		filter["status"] = query.Status
	}
	if query.AccessPolicy != "" {
		filter["access_policy"] = query.AccessPolicy
	}
	if query.Creator != "" {
		filter["creator"] = query.Creator
	}
	if query.IdempotencyKey != "" {
		filter["idempotency_key"] = query.IdempotencyKey
	}
	if !query.IdempotencyExpiredBefore.IsZero() {
		// 两个条件缺一不可：没有 $exists 的话，所有不带幂等键的记录
		// （绝大多数）都会被当成"过期"反复扫出来。
		filter["idempotency_key"] = bson.M{"$exists": true}
		filter["idempotency_expire_at"] = bson.M{"$lt": query.IdempotencyExpiredBefore}
	}
	if query.Shared != nil {
		if *query.Shared {
			filter["shared"] = true
		} else {
			// 字段 omitempty，老记录里根本没有这个键，
			// 因此「非共享」必须同时认 false 与不存在。
			filter["shared"] = bson.M{"$ne": true}
		}
	}
	if len(query.Tags) > 0 {
		// $all 而不是 $in：Query.Tags 是 AND 语义，资源必须同时带上全部标签。
		filter["tags"] = bson.M{"$all": query.Tags}
	}
	for key, value := range query.Metadata {
		// key 已由 Query.Validate 挡掉 "$" 与 "."，到这里拼成字段路径是安全的。
		filter["metadata."+key] = value
	}
	if !query.DeletedBefore.IsZero() {
		filter["$or"] = bson.A{
			bson.M{"deleted_at": bson.M{"$lt": query.DeletedBefore}},
			bson.M{"deleted_at": bson.M{"$exists": false}, "updated_at": bson.M{"$lt": query.DeletedBefore}},
		}
	}
	if !query.CreatedAfter.IsZero() || !query.CreatedBefore.IsZero() {
		rangeFilter := bson.M{}
		if !query.CreatedAfter.IsZero() {
			rangeFilter["$gte"] = query.CreatedAfter
		}
		if !query.CreatedBefore.IsZero() {
			rangeFilter["$lte"] = query.CreatedBefore
		}
		filter["created_at"] = rangeFilter
	}
	return filter
}

// wrapNotFound 把驱动层的“无文档”错误统一成 store.ErrNotFound。
func wrapNotFound(err error) error {
	if errors.Is(err, mongo.ErrNoDocuments) {
		return store.ErrNotFound
	}
	return err
}

// usageCol 用量集合，与资源集合分开存放。
// 用量是高频 $inc 的小文档，和资源元数据混在一起会让后者的
// 工作集被反复刷新的计数文档挤占。
func (s *MongoStore) usageCol() *mongo.Collection {
	return s.client.Database(s.database).Collection(s.collection + "_usage")
}

// AddUsage 原子增量。单条 $inc + upsert，不需要读改写，天然免疫并发。
func (s *MongoStore) AddUsage(ctx context.Context, namespace, accountID string, bytes, files int64) error {
	if bytes == 0 && files == 0 {
		return nil
	}
	filter := bson.M{"namespace": namespace, "account_id": accountID}
	update := bson.M{
		"$inc": bson.M{"bytes": bytes, "files": files},
		"$set": bson.M{"updated_at": time.Now()},
		// 首次 upsert 时补齐键字段，否则新文档里只有计数没有归属。
		"$setOnInsert": filter,
	}
	_, err := s.usageCol().UpdateOne(ctx, filter, update, options.Update().SetUpsert(true))
	return err
}

func (s *MongoStore) GetUsage(ctx context.Context, namespace, accountID string) (*model.Usage, error) {
	var usage model.Usage
	err := s.usageCol().FindOne(ctx, bson.M{"namespace": namespace, "account_id": accountID}).Decode(&usage)
	if errors.Is(err, mongo.ErrNoDocuments) {
		// 没有记录等同于零用量，不是错误。
		return &model.Usage{Namespace: namespace, AccountID: accountID}, nil
	}
	if err != nil {
		return nil, err
	}
	return &usage, nil
}

// EnsureUsageIndexes 为用量集合建唯一索引。
// 与 EnsureIndexes 分开：用量集合是可选能力，没开配额的部署不必建它。
func (s *MongoStore) EnsureUsageIndexes(ctx context.Context) error {
	_, err := s.usageCol().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "namespace", Value: 1}, {Key: "account_id", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	return err
}

// ComputeUsage 用聚合管道现算真实用量。
//
// 两段 $group 是必须的：第一段按 object_key 去重（去重策略 new_record 下
// 多条记录共享一个对象，直接 sum(size) 会把同一份字节算 N 次），
// 第二段才把去重后的结果累加。少一段就是超收。
//
// 代价是一次全扫该租户的记录。它只在对账任务里跑，不在请求路径上——
// 这正是增量计数存在的理由：日常读走 $inc 维护的那个数，
// 偏差靠这个方法定期校正。
func (s *MongoStore) ComputeUsage(ctx context.Context, namespace, accountID string) (*model.Usage, error) {
	match := bson.M{}
	if namespace != "" {
		match["namespace"] = namespace
	}
	if accountID != "" {
		match["account_id"] = accountID
	}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: match}},
		// 同一个对象取一次大小即可，用 $first 而不是 $sum。
		{{Key: "$group", Value: bson.M{
			"_id":  "$object_key",
			"size": bson.M{"$first": "$size"},
		}}},
		{{Key: "$group", Value: bson.M{
			"_id":   nil,
			"bytes": bson.M{"$sum": "$size"},
			"files": bson.M{"$sum": 1},
		}}},
	}
	cursor, err := s.col().Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	usage := &model.Usage{Namespace: namespace, AccountID: accountID, UpdatedAt: time.Now()}
	if cursor.Next(ctx) {
		var row struct {
			Bytes int64 `bson:"bytes"`
			Files int64 `bson:"files"`
		}
		if err = cursor.Decode(&row); err != nil {
			return nil, err
		}
		usage.Bytes = row.Bytes
		usage.Files = row.Files
	}
	return usage, cursor.Err()
}

var (
	_ store.Store      = (*MongoStore)(nil)
	_ store.QuotaStore = (*MongoStore)(nil)
)
