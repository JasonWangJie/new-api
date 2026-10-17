package common

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedisNamespaceZCountIsolation(t *testing.T) {
	server := miniredis.RunT(t)
	plain := redis.NewClient(&redis.Options{Addr: server.Addr()})
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		assert.NoError(t, client.Close())
		assert.NoError(t, plain.Close())
	})
	// Multiple installations must not create a second namespace on a command.
	client.AddHook(RedisNamespaceHook{Prefix: "tenant"})
	client.AddHook(RedisNamespaceHook{Prefix: "tenant"})
	ctx := t.Context()
	require.NoError(t, plain.ZAdd(ctx, "tenant:image-slots",
		&redis.Z{Score: 9, Member: "expired"},
		&redis.Z{Score: 10, Member: "boundary"},
		&redis.Z{Score: 11, Member: "active-a"},
		&redis.Z{Score: 20, Member: "active-b"},
		&redis.Z{Score: 21, Member: "later"},
	).Err())
	require.NoError(t, plain.ZAdd(ctx, "image-slots", &redis.Z{Score: 100, Member: "other-instance"}).Err())

	t.Run("direct preserves exclusive score bounds", func(t *testing.T) {
		command := client.ZCount(ctx, "image-slots", "(10", "20")
		count, err := command.Result()
		require.NoError(t, err)
		assert.EqualValues(t, 2, count)
		assert.Equal(t, []any{"zcount", "tenant:image-slots", "(10", "20"}, command.Args())
	})
	t.Run("pipeline preserves infinite bounds and existing namespace", func(t *testing.T) {
		var all, active *redis.IntCmd
		_, err := client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			all = pipe.ZCount(ctx, "image-slots", "-inf", "+inf")
			active = pipe.ZCount(ctx, "tenant:image-slots", "(10", "20")
			return nil
		})
		require.NoError(t, err)
		assert.EqualValues(t, 5, all.Val())
		assert.EqualValues(t, 2, active.Val())
		assert.Equal(t, []any{"zcount", "tenant:image-slots", "-inf", "+inf"}, all.Args())
		assert.Equal(t, []any{"zcount", "tenant:image-slots", "(10", "20"}, active.Args())
	})
	t.Run("unsupported commands remain isolated", func(t *testing.T) {
		err := client.Do(ctx, "rename", "image-slots", "outside").Err()
		require.ErrorContains(t, err, "unsupported namespaced Redis command: rename")
		_, err = client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, "pending-write", "must-not-be-written", 0)
			pipe.Do(ctx, "rename", "image-slots", "outside")
			return nil
		})
		require.ErrorContains(t, err, "unsupported namespaced Redis command: rename")
		assert.False(t, server.Exists("pending-write"))
		assert.False(t, server.Exists("tenant:pending-write"))
		assert.False(t, server.Exists("outside"))
		assert.False(t, server.Exists("tenant:outside"))
		assert.True(t, server.Exists("tenant:image-slots"))
		assert.True(t, server.Exists("image-slots"))
	})
}
