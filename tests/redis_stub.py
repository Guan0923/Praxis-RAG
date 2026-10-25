"""Thread-safe redis-py subset for transient task unit tests."""

import threading
import time

from redis.exceptions import WatchError


class MemoryRedis:
    def __init__(self, clock=time.time):
        self.clock = clock
        self.values = {}
        self.expires = {}
        self.versions = {}
        self.mutex = threading.RLock()
        self.broken = False

    def _check(self, key):
        if self.broken:
            raise ConnectionError("Redis unavailable")
        if self.expires.get(key, float("inf")) <= self.clock():
            self.values.pop(key, None)
            self.expires.pop(key, None)
            self.versions[key] = self.versions.get(key, 0) + 1

    def get(self, key):
        with self.mutex:
            self._check(key)
            return self.values.get(key)

    def set(self, key, value, ex=None, nx=False):
        with self.mutex:
            self._check(key)
            if nx and key in self.values:
                return False
            self.values[key] = value.encode() if isinstance(value, str) else value
            self.versions[key] = self.versions.get(key, 0) + 1
            if ex is not None:
                self.expires[key] = self.clock() + ex
            else:
                self.expires.pop(key, None)
            return True

    def delete(self, *keys):
        count = 0
        with self.mutex:
            for key in keys:
                self._check(key)
                count += key in self.values
                self.values.pop(key, None)
                self.expires.pop(key, None)
                self.versions[key] = self.versions.get(key, 0) + 1
        return count

    def pipeline(self):
        return MemoryPipeline(self)

    def close(self):
        pass


class MemoryPipeline:
    def __init__(self, client):
        self.client = client
        self.watched = {}
        self.pending = []
        self.in_multi = False

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.reset()

    def watch(self, *keys):
        with self.client.mutex:
            for key in keys:
                self.client._check(key)
                self.watched[key] = self.client.versions.get(key, 0)

    def unwatch(self):
        self.watched.clear()

    def get(self, key):
        return self.client.get(key)

    def multi(self):
        self.in_multi = True

    def set(self, key, value, **kwargs):
        self.pending.append((self.client.set, (key, value), kwargs))
        return self

    def delete(self, *keys):
        self.pending.append((self.client.delete, keys, {}))
        return self

    def execute(self):
        with self.client.mutex:
            for key, version in self.watched.items():
                self.client._check(key)
                if self.client.versions.get(key, 0) != version:
                    raise WatchError()
            results = [func(*args, **kwargs) for func, args, kwargs in self.pending]
            self.reset()
            return results

    def reset(self):
        self.watched.clear()
        self.pending.clear()
        self.in_multi = False
