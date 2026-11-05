/* No account, network, microphone or playback access. Uses synthetic silence. */
#include "station_native.h"
#include <stdio.h>

int main(int argc, char **argv) {
  if (argc != 2) {
    puts("NATIVE_ABI_ARGUMENT_FAILED");
    return 64;
  }
  char message[512] = {0};
  station_spotter *spotter = station_spotter_open(argv[1], message, sizeof(message));
  if (!spotter) {
    puts("NATIVE_ABI_OPEN_FAILED");
    return 1;
  }
  puts("NATIVE_ABI_OPEN_OK");
  int16_t silence[320] = {0};
  int result = 0;
  for (int i = 0; i < 4; i++) {
    int event = station_spotter_feed(spotter, silence, sizeof(silence), message,
                                     sizeof(message));
    if (event != 0) {
      puts(event < 0 ? "NATIVE_ABI_FEED_FAILED" : "NATIVE_ABI_SILENCE_FALSE_WAKE");
      result = 2;
      break;
    }
  }
  if (!result)
    puts("NATIVE_ABI_SILENCE_OK");
  if (station_spotter_close(spotter, message, sizeof(message)) != 0) {
    puts("NATIVE_ABI_CLOSE_FAILED");
    return 3;
  }
  puts("NATIVE_ABI_CLOSE_OK");
  return result;
}
