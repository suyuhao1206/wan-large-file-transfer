<template>
  <div class="upload-container">
    <el-upload
      class="upload-demo"
      drag
      action=""
      :auto-upload="false"
      :on-change="handleFileChange"
      :show-file-list="false"
    >
      <el-icon class="el-icon--upload"><upload-filled /></el-icon>
      <div class="el-upload__text">
        将文件拖到此处或 <em>点击上传</em>
      </div>
    </el-upload>

    <div v-if="file" class="file-status">
      <p>{{ file.name }} ({{ formatSize(file.size) }})</p>
      <el-progress :percentage="progress" />
      
      <div class="actions">
        <el-button type="primary" @click="startUpload" v-if="!uploading && !uploaded">
          开始上传
        </el-button>
        <el-button type="warning" @click="pauseUpload" v-if="uploading">
          暂停
        </el-button>
        <el-button type="primary" @click="resumeUpload" v-if="paused">
          继续
        </el-button>
      </div>
    </div>

    <div v-if="uploaded" class="result-box">
      <el-result
        icon="success"
        title="上传成功！"
        sub-title="您的文件已准备好分享"
      >
        <template #extra>
          <div class="code-display">
            <span>取件码:</span>
            <h1 class="code">{{ shareCode }}</h1>
          </div>
        </template>
      </el-result>
    </div>
  </div>
</template>

<script setup>
import { ref } from 'vue'
import * as tus from 'tus-js-client'
import axios from 'axios'
import { UploadFilled } from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import { getApiKey, getAdminKey } from '../config.js'

const file = ref(null)
const progress = ref(0)
const uploading = ref(false)
const paused = ref(false)
const uploaded = ref(false)
const shareCode = ref('')
let upload = null

const handleFileChange = (uploadFile) => {
  file.value = uploadFile.raw
  progress.value = 0
  uploaded.value = false
  uploading.value = false
  paused.value = false
  shareCode.value = ''
}

const formatSize = (bytes) => {
  if (bytes === 0) return '0 B'
  const k = 1024
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(k))
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i]
}

const startUpload = () => {
  if (!file.value) return

  uploading.value = true
  
  // Get API key or Admin key
  const apiKey = getApiKey()
  const adminKey = getAdminKey()
  
  // Prepare headers - prefer API key, fallback to Admin key
  const headers = {}
  if (apiKey) {
    headers['X-API-Key'] = apiKey
  } else if (adminKey) {
    headers['X-Admin-Key'] = adminKey
  }
  
  // 调试信息
  console.log('Upload headers:', headers)
  console.log('API Key:', apiKey)
  console.log('Admin Key:', adminKey)
  
  upload = new tus.Upload(file.value, {
    endpoint: '/files/',
    retryDelays: [0, 3000, 5000, 10000, 20000],
    headers: headers,
    metadata: {
      filename: file.value.name,
      filetype: file.value.type
    },
    onError: (error) => {
      console.error('Failed because: ' + error)
      ElMessage.error('上传失败: ' + error.message)
      uploading.value = false
    },
    onProgress: (bytesUploaded, bytesTotal) => {
      const percentage = (bytesUploaded / bytesTotal * 100).toFixed(2)
      progress.value = Number(percentage)
    },
    onSuccess: async () => {
      console.log('Download %s from %s', upload.file.name, upload.url)
      uploading.value = false
      uploaded.value = true
      
      // Get the upload ID from the URL
      const uploadUrl = upload.url
      const uploadId = uploadUrl.substring(uploadUrl.lastIndexOf('/') + 1)
      
      // Call API to generate share code (axios 拦截器会自动添加密钥)
      try {
          const res = await axios.post('/api/get-code', { upload_id: uploadId })
          shareCode.value = res.data.code
        } catch (err) {
          console.error('Generate share code error:', err)
          ElMessage.error('生成取件码失败')
      }
    }
  })

  // Check if there are any previous uploads to continue.
  upload.findPreviousUploads().then((previousUploads) => {
    // Ask the user for their preference.
    if (previousUploads.length) {
      upload.resumeFromPreviousUpload(previousUploads[0])
    }
    upload.start()
  })
}

const pauseUpload = () => {
  if (upload) {
    upload.abort()
    uploading.value = false
    paused.value = true
  }
}

const resumeUpload = () => {
  if (upload) {
    upload.start()
    uploading.value = true
    paused.value = false
  }
}
</script>

<style scoped>
.upload-container {
  text-align: center;
  padding: 20px;
}
.file-status {
  margin-top: 20px;
}
.actions {
  margin-top: 15px;
}
.code-display {
  margin-top: 10px;
  background: #f0f9eb;
  padding: 15px;
  border-radius: 8px;
  border: 1px solid #67c23a;
}
.code {
  color: #67c23a;
  font-size: 18px;
  margin: 10px 0;
  word-break: break-all;
  font-family: monospace;
}
</style>
