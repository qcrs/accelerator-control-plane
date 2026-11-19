/* Minimal read-only runtime management endpoint. No session or device context
 * is allocated. Copy this header into each runtime's server/ directory.
 * Management counts include admitted handshakes and cleanup in progress. */
#ifndef ENDPOINT_OBSERVATION_H
#define ENDPOINT_OBSERVATION_H
#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <time.h>
#include <unistd.h>

static int observation_listen(const char *env_name, const char *bind_host) {
    const char *port = getenv(env_name);
    if (!port || !*port) return -1; /* opt-in; old servers remain compatible */
    char *end; long n = strtol(port, &end, 10);
    if (*end || n < 1 || n > 65535) return -2;
    int fd = socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
    if (fd < 0) return -2;
    int yes = 1; setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &yes, sizeof(yes));
    struct sockaddr_in addr; memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET; addr.sin_port = htons((unsigned short)n);
    if (inet_pton(AF_INET, bind_host, &addr.sin_addr) != 1 ||
        bind(fd, (struct sockaddr *)&addr, sizeof(addr)) || listen(fd, 8) ||
        fcntl(fd, F_SETFL, O_NONBLOCK)) { close(fd); return -2; }
    return fd;
}

static int observation_request(int listener) {
    int fd = accept(listener, NULL, NULL);
    if (fd < 0) return -1;
    struct timeval timeout = {.tv_sec = 0, .tv_usec = 500000};
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));
    char request[1024]; size_t used = 0;
    struct timespec ts; clock_gettime(CLOCK_MONOTONIC, &ts);
    long long deadline = (long long)ts.tv_sec * 1000 + ts.tv_nsec / 1000000 + 500;
    while (used < sizeof(request)-1) {
        clock_gettime(CLOCK_MONOTONIC, &ts);
        long long remaining = deadline - ((long long)ts.tv_sec * 1000 + ts.tv_nsec / 1000000);
        if (remaining <= 0) break;
        struct pollfd p = {.fd=fd, .events=POLLIN};
        if (poll(&p, 1, (int)remaining) <= 0) break;
        ssize_t n = recv(fd, request+used, sizeof(request)-1-used, MSG_DONTWAIT);
        if (n <= 0) break;
        used += (size_t)n; request[used] = 0;
        /* Consume all headers before replying and closing. Leaving header
         * bytes unread can reset a fragmented TCP request after the reply. */
        if (strstr(request, "\r\n\r\n")) {
            if (strncmp(request, "GET /v1/observation HTTP/1.", 27) == 0) return fd;
            break;
        }
    }
    close(fd); return -1;
}

static void observation_send_close(int fd, const char *reply, size_t length) {
    size_t offset = 0;
    while (offset < length) {
        ssize_t sent = send(fd, reply+offset, length-offset, MSG_NOSIGNAL);
        if (sent < 0 && errno == EINTR) continue;
        if (sent <= 0) break;
        offset += (size_t)sent;
    }
    close(fd);
}

static inline void observation_busy(int fd) {
    const char reply[] = "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n";
    observation_send_close(fd, reply, sizeof(reply)-1);
}

static void observation_reply(int fd, const char *driver, int healthy,
                              int capacity, unsigned sessions, unsigned cleanup_errors) {
    char body[512], reply[768];
    int n = snprintf(body, sizeof(body),
        "{\"schemaVersion\":1,\"driver\":\"%s\",\"healthy\":%s,\"capacity\":%d,\"activeSessions\":%u,\"cleanupErrors\":%u}\n",
        driver, healthy ? "true" : "false", capacity, sessions, cleanup_errors);
    int length = snprintf(reply, sizeof(reply),
        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nCache-Control: no-store\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", n, body);
    observation_send_close(fd, reply, (size_t)length);
}
#endif
