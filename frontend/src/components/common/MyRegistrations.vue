<template>
  <el-table :data="list" v-loading="loading" border>
    <el-table-column prop="id" label="ID" width="70" />
    <el-table-column prop="activity_id" label="活动ID" width="90" />
    <el-table-column prop="voucher_no" label="凭证号" width="180">
      <template #default="{ row }">
        <span v-if="row.status === 'waitlisted'">—（候补转正后生效）</span>
        <span v-else>{{ row.voucher_no }}</span>
      </template>
    </el-table-column>
    <el-table-column label="报名状态" width="160">
      <template #default="{ row }">
        <el-tag :type="tag(row.status)">{{ RegistrationStatusText[row.status] }}</el-tag>
        <el-tag v-if="row.status === 'waitlisted'" type="warning" size="small" class="pos-tag">
          候补第 {{ row.waitlist_position || '-' }} 位
        </el-tag>
      </template>
    </el-table-column>
    <el-table-column label="审核" width="100">
      <template #default="{ row }">
        <el-tag :type="reviewTag(row.review_status)">{{ ReviewStatusText[row.review_status] }}</el-tag>
      </template>
    </el-table-column>
    <el-table-column prop="created_at" label="报名时间" />
    <el-table-column label="操作" width="100">
      <template #default="{ row }">
        <el-button
          v-if="row.status === 'registered' || row.status === 'waitlisted'"
          size="small"
          type="danger"
          @click="cancel(row)"
        >取消</el-button>
      </template>
    </el-table-column>
  </el-table>
  <el-pagination
    class="mt-2"
    layout="total, prev, pager, next"
    :total="total"
    :page-size="pageSize"
    @current-change="onPage"
  />
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { cancelRegistration, listMyRegistrations } from '@/api/registration'
import { RegistrationStatusText, ReviewStatusText } from '@/constants/registration'
import type { Registration } from '@/types'

const list = ref<Registration[]>([])
const total = ref(0)
const loading = ref(false)
const page = ref(1)
const pageSize = 10

async function load() {
  loading.value = true
  try {
    const res: any = await listMyRegistrations({ page: page.value, page_size: pageSize })
    list.value = res.data.list
    total.value = res.data.total
  } finally {
    loading.value = false
  }
}

async function cancel(row: Registration) {
  const tip = row.status === 'waitlisted'
    ? '确认退出候补队列？'
    : '确认取消该报名？取消后名额将顺延给候补用户。'
  await ElMessageBox.confirm(tip, '提示')
  await cancelRegistration(row.id)
  ElMessage.success(row.status === 'waitlisted' ? '已退出候补' : '已取消')
  await load()
}

function onPage(p: number) {
  page.value = p
  load()
}
function tag(status: string): string {
  if (status === 'checked_in') return 'success'
  if (status === 'cancelled') return 'info'
  if (status === 'waitlisted') return 'warning'
  return 'primary'
}
function reviewTag(status: string): string {
  if (status === 'approved') return 'success'
  if (status === 'rejected') return 'danger'
  return 'warning'
}

onMounted(load)
</script>

<style scoped>
.mt-2 { margin-top: 12px; }
.pos-tag { margin-left: 6px; }
</style>
