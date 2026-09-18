#ifndef INTERBASE_GO_SERVICES_NATIVE_H
#define INTERBASE_GO_SERVICES_NATIVE_H

#include <stddef.h>
#include <stdint.h>

typedef struct ib_service_manager ib_service_manager;

ib_service_manager *ib_service_open(const char *name, size_t name_length,
	const unsigned char *spb, size_t spb_length, char **error, int *failed);
int ib_service_start(ib_service_manager *manager, const unsigned char *request,
	size_t request_length, char **error);
int ib_service_query(ib_service_manager *manager, unsigned char item,
	size_t capacity, unsigned char **result, size_t *result_length,
	int *truncated, char **error);
int ib_service_close(ib_service_manager *manager, char **error);
void ib_service_buffer_free(unsigned char *buffer);
void ib_service_error_free(char *error);

#endif
