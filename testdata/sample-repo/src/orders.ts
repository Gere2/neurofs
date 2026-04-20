import { db } from '../config/database'
import { ProductController } from './products'

export interface Order {
  id: string
  userId: string
  items: OrderItem[]
  total: number
  status: 'pending' | 'confirmed' | 'shipped' | 'delivered'
  createdAt: Date
}

export interface OrderItem {
  productId: string
  quantity: number
  price: number
}

export class OrderController {
  static async list(req: any, res: any) {
    const orders = await db.query(
      'SELECT * FROM orders WHERE user_id = $1',
      [req.user.id]
    )
    res.json(orders)
  }

  static async create(req: any, res: any) {
    const { items } = req.body
    let total = 0

    for (const item of items) {
      const product = await ProductController.findById(item.productId)
      if (!product || product.stock < item.quantity) {
        return res.status(400).json({ error: 'Product unavailable' })
      }
      total += product.price * item.quantity
    }

    const order = await db.query(
      'INSERT INTO orders (user_id, items, total, status) VALUES ($1, $2, $3, $4) RETURNING *',
      [req.user.id, JSON.stringify(items), total, 'pending']
    )

    // Adjust stock
    for (const item of items) {
      await ProductController.updateStock(item.productId, -item.quantity)
    }

    res.status(201).json(order)
  }
}
