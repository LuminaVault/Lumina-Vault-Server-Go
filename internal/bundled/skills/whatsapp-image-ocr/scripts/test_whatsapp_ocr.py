#!/usr/bin/env python3
"""
Test script for WhatsApp Image OCR skill.
"""

import os
import sys
import json
from pathlib import Path

# Add parent directory to path
sys.path.append(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from hermes_tools.read_file import read_file
from hermes_tools.write_file import write_file
from hermes_tools.execute_code import execute_code

def test_ocr():
    """Test the OCR functionality."""
    print("Testing WhatsApp Image OCR...")
    
    # Test image path (you can change this for your setup)
    test_image = Path("/tmp/test_image.jpg")
    
    if not test_image.exists():
        print(f"Test image not found: {test_image}")
        print("Creating a sample test image...")
        # Create a simple test image with text
        try:
            # Use ImageMagick to create a test image
            result = execute_code(
                code="""
from PIL import Image, ImageDraw, ImageFont
import io
import textwrap

# Create a simple image with text
img = Image.new('RGB', (400, 200), color='white')
draw = ImageDraw.Draw(img)

# Try to use a default font
try:
    # For Linux
    font = ImageFont.truetype("/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", 20)
except:
    try:
        # For Mac
        font = ImageFont.truetype("/Library/Fonts/DejaVuSans.ttf", 20)
    except:
        font = ImageFont.load_default()

# Add some text
text = "Hello World! This is a test."
draw.text((10, 50), text, fill='black', font=font)

# Save to file
img.save('/tmp/test_image.jpg')
"""
            )
            if result['exit_code'] != 0:
                print("Failed to create test image. Please create a test image manually.")
                return 1
        except:
            print("Failed to create test image. Please create a test image manually.")
            return 1
    
    # Run OCR on the test image
    print(f"Running OCR on test image: {test_image}")
    
    # Use the OCR processor
    from whatsapp_image_ocr import WhatsAppImageOCR
    ocr_processor = WhatsAppImageOCR()
    
    # Extract text
    extracted_text = ocr_processor.extract_text_with_ocr(str(test_image))
    print(f"Extracted text:\n---\n{extracted_text}\n---")
    
    if extracted_text and "Hello World" in extracted_text:
        print("✓ OCR test passed!")
        return 0
    else:
        print("✗ OCR test failed. Check output above.")
        return 1

if __name__ == '__main__':
    sys.exit(test_ocr())