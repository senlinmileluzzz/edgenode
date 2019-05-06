/*******************************************************************************
* Copyright 2019 Intel Corporation. All rights reserved.
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*
*     http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
*******************************************************************************/

#include <CUnit/CUnit.h>
#include "nes_ring_lookup.h"

int init_suite_nes_ring_lookup(void);
int cleanup_suite_nes_ring_lookup(void);

extern CU_TestInfo tests_suite_nes_ring_lookup[];
void nes_ring_add(const char *name, nes_ring_t *entry);
void nes_ring_del(const char *name);

struct nes_rings_bak_s {
	const char *name;
	nes_ring_t *ring;
};