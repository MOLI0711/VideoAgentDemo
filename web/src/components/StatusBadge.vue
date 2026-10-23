<script setup lang="ts">
import { statusLabel } from '../lib/timeline'
defineProps<{ status: string }>()
</script>
<template>
  <span class="badge" :class="status.toLowerCase()">{{ statusLabel(status) }}</span>
</template>
