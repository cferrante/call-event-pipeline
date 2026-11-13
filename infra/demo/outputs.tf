output "sut_id" {
  value = aws_instance.this["sut"].id
}

output "loadgen_id" {
  value = aws_instance.this["loadgen"].id
}

output "sut_private_ip" {
  value = aws_instance.this["sut"].private_ip
}

output "bucket" {
  value = aws_s3_bucket.artifacts.id
}

output "availability_zone" {
  value = data.aws_subnet.chosen.availability_zone
}
