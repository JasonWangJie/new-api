import fs from 'node:fs/promises'
import path from 'node:path'

const LOCALES_DIR = path.resolve('src/i18n/locales')

function stableStringify(obj) {
  return JSON.stringify(obj, null, 2) + '\n'
}

const newKeys = {
  en: {
    'Back to documentation': 'Back to documentation',
    Breadcrumb: 'Breadcrumb',
    Contents: 'Contents',
    'Documentation navigation': 'Documentation navigation',
    'Page not found': 'Page not found',
    'The requested documentation page does not exist.':
      'The requested documentation page does not exist.',
  },
  zh: {
    'Back to documentation': '返回文档',
    Breadcrumb: '面包屑导航',
    Contents: '目录',
    'Documentation navigation': '文档导航',
    'Page not found': '页面未找到',
    'The requested documentation page does not exist.':
      '请求的文档页面不存在。',
  },
  'zh-TW': {
    'Back to documentation': '返回文件',
    Breadcrumb: '麵包屑導覽',
    Contents: '目錄',
    'Documentation navigation': '文件導覽',
    'Page not found': '找不到頁面',
    'The requested documentation page does not exist.':
      '請求的文件頁面不存在。',
  },
  fr: {
    'Back to documentation': 'Retour à la documentation',
    Breadcrumb: 'Fil d’Ariane',
    Contents: 'Sommaire',
    'Documentation navigation': 'Navigation de la documentation',
    'Page not found': 'Page introuvable',
    'The requested documentation page does not exist.':
      "La page de documentation demandée n'existe pas.",
  },
  ja: {
    'Back to documentation': 'ドキュメントに戻る',
    Breadcrumb: 'パンくずリスト',
    Contents: '目次',
    'Documentation navigation': 'ドキュメントナビ',
    'Page not found': 'ページが見つかりません',
    'The requested documentation page does not exist.':
      '指定されたドキュメントページは存在しません。',
  },
  ru: {
    'Back to documentation': 'Вернуться к документации',
    Breadcrumb: 'Навигационная цепочка',
    Contents: 'Содержание',
    'Documentation navigation': 'Навигация по документации',
    'Page not found': 'Страница не найдена',
    'The requested documentation page does not exist.':
      'Запрошенная страница документации не существует.',
  },
  vi: {
    'Back to documentation': 'Quay lại tài liệu',
    Breadcrumb: 'Đường dẫn',
    Contents: 'Mục lục',
    'Documentation navigation': 'Điều hướng tài liệu',
    'Page not found': 'Không tìm thấy trang',
    'The requested documentation page does not exist.':
      'Trang tài liệu được yêu cầu không tồn tại.',
  },
}

async function main() {
  let totalAdded = 0

  for (const [locale, trans] of Object.entries(newKeys)) {
    const filePath = path.join(LOCALES_DIR, `${locale}.json`)
    const json = JSON.parse(await fs.readFile(filePath, 'utf8'))

    let count = 0
    for (const [key, value] of Object.entries(trans)) {
      if (!(key in json.translation)) {
        json.translation[key] = value
        count++
      } else {
        json.translation[key] = value
      }
    }

    const sorted = Object.keys(json.translation)
      .sort((a, b) => a.localeCompare(b))
      .reduce((acc, key) => {
        acc[key] = json.translation[key]
        return acc
      }, {})

    json.translation = sorted
    await fs.writeFile(filePath, stableStringify(json), 'utf8')
    totalAdded += count
    console.log(`${locale}: wrote ${count} new keys`)
  }

  console.log(`Done. Added ${totalAdded} new key slots.`)
}

main().catch((error) => {
  console.error(error)
  process.exit(1)
})
