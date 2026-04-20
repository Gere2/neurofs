import { db } from '../config/database'

export interface Product {
  id: string
  name: string
  price: number
  stock: number
  category: string
}

export class ProductController {
  static async list(req: any, res: any) {
    const products = await db.query('SELECT * FROM products ORDER BY name')
    res.json(products)
  }

  static async create(req: any, res: any) {
    const { name, price, stock, category } = req.body
    const product = await db.query(
      'INSERT INTO products (name, price, stock, category) VALUES ($1, $2, $3, $4) RETURNING *',
      [name, price, stock, category]
    )
    res.status(201).json(product)
  }

  static async findById(id: string): Promise<Product | null> {
    return db.query('SELECT * FROM products WHERE id = $1', [id])
  }

  static async updateStock(id: string, delta: number): Promise<void> {
    await db.query(
      'UPDATE products SET stock = stock + $1 WHERE id = $2',
      [delta, id]
    )
  }
}
