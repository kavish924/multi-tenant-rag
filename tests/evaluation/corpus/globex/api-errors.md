# Public API error codes

The Globex public API returns stable error codes. ERR-4012 means the API token has expired; create a new token in the developer console. ERR-4291 means the client exceeded the rate limit; wait for the number of seconds in the Retry-After header. ERR-5003 means an upstream dependency timed out; retry with exponential backoff. Error responses always include a request id for support.
