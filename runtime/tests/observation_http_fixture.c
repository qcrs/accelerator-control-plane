#include "../endpoint_observation.h"

int main(int argc, char **argv) {
    if (argc != 2 || setenv("TEST_OBSERVATION_PORT", argv[1], 1)) return 2;
    int listener = observation_listen("TEST_OBSERVATION_PORT", "127.0.0.1");
    if (listener < 0) return 3;
    puts("READY"); fflush(stdout);
    struct pollfd ready = {.fd=listener, .events=POLLIN};
    if (poll(&ready, 1, 3000) <= 0) return 4;
    int fd = observation_request(listener);
    if (fd >= 0) observation_reply(fd, "corex-remote", 1, 1, 0, 0);
    close(listener);
    return 0;
}
