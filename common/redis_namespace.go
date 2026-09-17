package common

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/go-redis/redis/v8"
)

// RedisNamespaceHook isolates an entire application instance sharing Redis.
// Unsupported commands fail closed so an isolated instance cannot accidentally
// write an unprefixed key. The default installation has no hook.
type RedisNamespaceHook struct{ Prefix string }

func (hook RedisNamespaceHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	args := cmd.Args()
	var positions []int
	switch strings.ToLower(cmd.Name()) {
	case "ping", "info", "multi", "exec", "discard":
	case "get", "getset", "set", "setnx", "setex", "hgetall", "hset", "hget", "hincrby", "hdel", "expire", "pexpire", "ttl", "pttl", "zadd", "zrem", "zrange", "zscore", "zrangebyscore", "zcard", "zremrangebyscore", "incr", "incrby", "decrby":
		positions = []int{1}
	case "del", "mget", "exists", "watch":
		for i := 1; i < len(args); i++ {
			positions = append(positions, i)
		}
	case "eval", "evalsha":
		if len(args) < 3 {
			return ctx, errors.New("invalid namespaced Redis script")
		}
		count, err := strconv.Atoi(redisNamespaceArgument(args[2]))
		if err != nil || count < 0 || count > len(args)-3 {
			return ctx, errors.New("invalid namespaced Redis script keys")
		}
		for i := 3; i < 3+count; i++ {
			positions = append(positions, i)
		}
	case "script":
		if len(args) < 2 || !strings.EqualFold(redisNamespaceArgument(args[1]), "load") {
			return ctx, errors.New("unsupported namespaced Redis script command")
		}
	case "scan":
		scoped := false
		for i := 2; i+1 < len(args); i++ {
			if strings.EqualFold(redisNamespaceArgument(args[i]), "match") && strings.HasPrefix(redisNamespaceArgument(args[i+1]), hook.Prefix+":") {
				scoped = true
			}
		}
		if !scoped {
			return ctx, errors.New("Redis scan requires the configured namespace")
		}
	default:
		return ctx, errors.New("unsupported namespaced Redis command: " + cmd.Name())
	}
	for _, position := range positions {
		if position >= len(args) {
			return ctx, errors.New("invalid namespaced Redis key")
		}
		key := redisNamespaceArgument(args[position])
		if !strings.HasPrefix(key, hook.Prefix+":") {
			args[position] = hook.Prefix + ":" + key
		}
	}
	return ctx, nil
}
func redisNamespaceArgument(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case int:
		return strconv.Itoa(value)
	case int64:
		return strconv.FormatInt(value, 10)
	}
	return ""
}
func (hook RedisNamespaceHook) AfterProcess(context.Context, redis.Cmder) error { return nil }
func (hook RedisNamespaceHook) BeforeProcessPipeline(ctx context.Context, commands []redis.Cmder) (context.Context, error) {
	for _, cmd := range commands {
		if _, err := hook.BeforeProcess(ctx, cmd); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}
func (hook RedisNamespaceHook) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }
