/* Model-format probe, not a Kolya detector. No microphone/network/account code. */
#define _POSIX_C_SOURCE 200809L
#include <dlfcn.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/resource.h>
#include <time.h>

typedef struct { float scale; int32_t zero_point; } Quantization;
typedef struct {
  const char *(*version)(void);
  void *(*model)(const char *);
  void (*model_delete)(void *);
  void *(*options)(void);
  void (*options_delete)(void *);
  void (*threads)(void *, int32_t);
  void (*reporter)(void *, void (*)(void *, const char *, va_list), void *);
  void *(*create)(const void *, const void *);
  void (*destroy)(void *);
  int (*allocate)(void *);
  int (*invoke)(void *);
  int32_t (*inputs)(const void *);
  int32_t (*outputs)(const void *);
  void *(*input)(const void *, int32_t);
  const void *(*output)(const void *, int32_t);
  int (*type)(const void *);
  int32_t (*rank)(const void *);
  int32_t (*dim)(const void *, int32_t);
  size_t (*bytes)(const void *);
  Quantization (*quant)(const void *);
  int (*copy_in)(void *, const void *, size_t);
  int (*copy_out)(const void *, void *, size_t);
} API;

static void discard_report(void *user, const char *format, va_list args) {
  (void)format; (void)args;
  (*(unsigned *)user)++;
}

#define SYMBOL(member, name) do { \
  *(void **)(&api.member) = dlsym(library, name); \
  if (!api.member) { puts("KWS_API_SYMBOL_MISSING"); goto done; } \
} while (0)

int main(int argc, char **argv) {
  if (argc < 2 || argc > 3) {
    puts("KWS_ARGUMENT_FAILED");
    return 64;
  }
  int result = 1;
  void *library = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
  if (!library) { puts("KWS_LIBRARY_LOAD_FAILED"); return 1; }
  API api = {0};
  void *model = NULL, *options = NULL, *interpreter = NULL;
  unsigned errors = 0;
  SYMBOL(version, "TfLiteVersion");
  const char *version = api.version();
  if (version && *version && strlen(version) <= 24 &&
      strspn(version, "0123456789.") == strlen(version))
    printf("KWS_RUNTIME_VERSION %s\n", version);
  else
    puts("KWS_RUNTIME_VERSION_UNCLASSIFIED");
  if (argc == 2) { result = 0; goto done; }
  SYMBOL(model, "TfLiteModelCreateFromFile");
  SYMBOL(model_delete, "TfLiteModelDelete");
  SYMBOL(options, "TfLiteInterpreterOptionsCreate");
  SYMBOL(options_delete, "TfLiteInterpreterOptionsDelete");
  SYMBOL(threads, "TfLiteInterpreterOptionsSetNumThreads");
  SYMBOL(reporter, "TfLiteInterpreterOptionsSetErrorReporter");
  SYMBOL(create, "TfLiteInterpreterCreate");
  SYMBOL(destroy, "TfLiteInterpreterDelete");
  SYMBOL(allocate, "TfLiteInterpreterAllocateTensors");
  SYMBOL(invoke, "TfLiteInterpreterInvoke");
  SYMBOL(inputs, "TfLiteInterpreterGetInputTensorCount");
  SYMBOL(outputs, "TfLiteInterpreterGetOutputTensorCount");
  SYMBOL(input, "TfLiteInterpreterGetInputTensor");
  SYMBOL(output, "TfLiteInterpreterGetOutputTensor");
  SYMBOL(type, "TfLiteTensorType");
  SYMBOL(rank, "TfLiteTensorNumDims");
  SYMBOL(dim, "TfLiteTensorDim");
  SYMBOL(bytes, "TfLiteTensorByteSize");
  SYMBOL(quant, "TfLiteTensorQuantizationParams");
  SYMBOL(copy_in, "TfLiteTensorCopyFromBuffer");
  SYMBOL(copy_out, "TfLiteTensorCopyToBuffer");
  model = api.model(argv[2]);
  if (!model) { puts("KWS_MODEL_LOAD_FAILED"); goto done; }
  options = api.options();
  if (!options) { puts("KWS_OPTIONS_FAILED"); goto done; }
  api.threads(options, 1);
  api.reporter(options, discard_report, &errors);
  interpreter = api.create(model, options);
  if (!interpreter) { puts("KWS_INTERPRETER_CREATE_FAILED"); goto done; }
  if (api.allocate(interpreter) != 0) {
    puts("KWS_MODEL_ALLOCATION_FAILED"); goto done;
  }
  if (api.inputs(interpreter) != 1 || api.outputs(interpreter) != 1) {
    puts("KWS_MODEL_TENSOR_COUNT_FAILED"); goto done;
  }
  void *input = api.input(interpreter, 0);
  const void *output = api.output(interpreter, 0);
  if (!input || !output) { puts("KWS_MODEL_TENSOR_FAILED"); goto done; }
  int input_type = api.type(input), output_type = api.type(output);
  if ((input_type != 3 && input_type != 9) ||
      (output_type != 3 && output_type != 9) ||
      api.rank(input) != 3 || api.dim(input, 0) != 1 ||
      api.dim(input, 1) < 1 || api.dim(input, 1) > 10 ||
      api.dim(input, 2) != 40 ||
      api.bytes(input) != (size_t)api.dim(input, 1) * 40 ||
      api.bytes(output) != 1) {
    puts("KWS_MODEL_SHAPE_OR_TYPE_FAILED"); goto done;
  }
  Quantization iq = api.quant(input), oq = api.quant(output);
  if (!(iq.scale > 0) || !(oq.scale > 0) ||
      iq.zero_point < (input_type == 9 ? -128 : 0) ||
      iq.zero_point > (input_type == 9 ? 127 : 255)) {
    puts("KWS_MODEL_QUANTIZATION_FAILED"); goto done;
  }
  printf("KWS_MODEL_QUANTIZED_OK stride=%d features=40\n", api.dim(input, 1));
  unsigned char feature_zero[400], probability[1];
  memset(feature_zero, (unsigned char)iq.zero_point, sizeof(feature_zero));
  struct timespec before, after;
  if (clock_gettime(CLOCK_MONOTONIC, &before) != 0) {
    puts("KWS_CLOCK_FAILED"); goto done;
  }
  for (int i = 0; i < 100; i++) {
    if (api.copy_in(input, feature_zero, api.bytes(input)) != 0 ||
        api.invoke(interpreter) != 0 ||
        api.copy_out(output, probability, sizeof(probability)) != 0) {
      puts("KWS_SYNTHETIC_INFERENCE_FAILED"); goto done;
    }
  }
  if (clock_gettime(CLOCK_MONOTONIC, &after) != 0) {
    puts("KWS_CLOCK_FAILED"); goto done;
  }
  long long elapsed = ((long long)after.tv_sec - before.tv_sec) * 1000000 +
                       (after.tv_nsec - before.tv_nsec) / 1000;
  printf("KWS_SYNTHETIC_INFERENCE_OK iterations=100 elapsed_us=%lld\n", elapsed);
  struct rusage usage;
  if (getrusage(RUSAGE_SELF, &usage) == 0)
    printf("KWS_PROCESS_MAXRSS_KIB %ld\n", usage.ru_maxrss);
  if (errors) { puts("KWS_RUNTIME_REPORTED_ERRORS"); goto done; }
  result = 0;
done:
  if (interpreter) api.destroy(interpreter);
  if (options) api.options_delete(options);
  if (model) api.model_delete(model);
  dlclose(library);
  return result;
}
